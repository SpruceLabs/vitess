/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtctlbackup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"

	"vitess.io/vitess/go/test/endtoend/sharding/initialsharding"

	"vitess.io/vitess/go/vt/proto/topodata"

	"github.com/stretchr/testify/assert"
	"vitess.io/vitess/go/test/endtoend/cluster"
)

var (
	master           *cluster.Vttablet
	replica1         *cluster.Vttablet
	replica2         *cluster.Vttablet
	localCluster     *cluster.LocalProcessCluster
	newInitDBFile    string
	useXtrabackup    bool
	xbStreamMode     string
	xbStripes        int
	cell             = cluster.DefaultCell
	hostname         = "localhost"
	keyspaceName     = "ks"
	dbPassword       = "VtDbaPass"
	shardKsName      = fmt.Sprintf("%s/%s", keyspaceName, shardName)
	dbCredentialFile string
	shardName        = "0"
	commonTabletArg  = []string{
		"-vreplication_healthcheck_topology_refresh", "1s",
		"-vreplication_healthcheck_retry_delay", "1s",
		"-vreplication_retry_delay", "1s",
		"-degraded_threshold", "5s",
		"-lock_tables_timeout", "5s",
		"-watch_replication_stream",
		"-enable_replication_reporter",
		"-serving_state_grace_period", "1s",
	}

	vtInsertTest = `
					create table vt_insert_test (
					  id bigint auto_increment,
					  msg varchar(64),
					  primary key (id)
					  ) Engine=InnoDB`
)

func LaunchCluster(xtrabackup bool, streamMode string, stripes int) (int, error) {
	localCluster = cluster.NewCluster(cell, hostname)

	// Start topo server
	err := localCluster.StartTopo()
	if err != nil {
		return 1, err
	}

	// Start keyspace
	keyspace := &cluster.Keyspace{
		Name: keyspaceName,
	}
	localCluster.Keyspaces = append(localCluster.Keyspaces, *keyspace)

	dbCredentialFile = initialsharding.WriteDbCredentialToTmp(localCluster.TmpDirectory)
	initDb, _ := ioutil.ReadFile(path.Join(os.Getenv("VTROOT"), "/config/init_db.sql"))
	sql := string(initDb)
	newInitDBFile = path.Join(localCluster.TmpDirectory, "init_db_with_passwords.sql")
	sql = sql + initialsharding.GetPasswordUpdateSQL(localCluster)
	err = ioutil.WriteFile(newInitDBFile, []byte(sql), 0666)
	if err != nil {
		return 1, err
	}

	extraArgs := []string{"-db-credentials-file", dbCredentialFile}
	commonTabletArg = append(commonTabletArg, "-db-credentials-file", dbCredentialFile)

	// Update arguments for xtrabackup
	if xtrabackup {
		useXtrabackup = xtrabackup
		xbStreamMode = streamMode
		xbStripes = stripes

		xtrabackupArgs := []string{
			"-backup_engine_implementation", "xtrabackup",
			fmt.Sprintf("-xtrabackup_stream_mode=%s", xbStreamMode),
			"-xtrabackup_user=vt_dba",
			fmt.Sprintf("-xtrabackup_stripes=%d", xbStripes),
			"-xtrabackup_backup_flags", fmt.Sprintf("--password=%s", dbPassword),
		}

		commonTabletArg = append(commonTabletArg, xtrabackupArgs...)
	}

	shard := cluster.Shard{
		Name: shardName,
	}

	var mysqlProcs []*exec.Cmd
	for i := 0; i < 3; i++ {
		tabletType := "replica"
		if i == 0 {
			tabletType = "master"
		}
		tablet := localCluster.GetVttabletInstance(tabletType, 0, cell)
		tablet.VttabletProcess = localCluster.GetVtprocessInstanceFromVttablet(tablet, shard.Name, keyspaceName)
		tablet.VttabletProcess.DbPassword = dbPassword
		tablet.VttabletProcess.ExtraArgs = commonTabletArg
		tablet.VttabletProcess.SupportsBackup = true
		tablet.VttabletProcess.EnableSemiSync = true

		tablet.MysqlctlProcess = *cluster.MysqlCtlProcessInstance(tablet.TabletUID, tablet.MySQLPort, localCluster.TmpDirectory)
		tablet.MysqlctlProcess.InitDBFile = newInitDBFile
		tablet.MysqlctlProcess.ExtraArgs = extraArgs
		if proc, err := tablet.MysqlctlProcess.StartProcess(); err != nil {
			return 1, err
		} else {
			mysqlProcs = append(mysqlProcs, proc)
		}
		shard.Vttablets = append(shard.Vttablets, tablet)
	}
	for _, proc := range mysqlProcs {
		if err := proc.Wait(); err != nil {
			return 1, err
		}
	}
	master = shard.Vttablets[0]
	replica1 = shard.Vttablets[1]
	replica2 = shard.Vttablets[2]

	if err := localCluster.VtctlclientProcess.InitTablet(master, cell, keyspaceName, hostname, shard.Name); err != nil {
		return 1, err
	}
	if err := localCluster.VtctlclientProcess.InitTablet(replica1, cell, keyspaceName, hostname, shard.Name); err != nil {
		return 1, err
	}

	for _, tablet := range []cluster.Vttablet{*master, *replica1} {
		if err := tablet.VttabletProcess.CreateDB(keyspaceName); err != nil {
			return 1, err
		}
		if err := tablet.VttabletProcess.Setup(); err != nil {
			return 1, err
		}
	}

	if err := localCluster.VtctlclientProcess.InitShardMaster(keyspaceName, shard.Name, cell, master.TabletUID); err != nil {
		return 1, err
	}
	return 0, nil
}

func TearDownCluster() {
	localCluster.Teardown()
}

func TestBackup(t *testing.T) {
	// Run all the backup tests
	t.Run("TestReplicaBackup", func(t *testing.T) {
		vtctlBackup(t, "replica")
	})

	t.Run("TestRdonlyBackup", func(t *testing.T) {
		vtctlBackup(t, "rdonly")
	})

	t.Run("TestMasterBackup", func(t *testing.T) {
		masterBackup(t)
	})

	t.Run("TestMasterReplicaSameBackup", func(t *testing.T) {
		masterReplicaSameBackup(t)
	})

	t.Run("TestRestoreOldMasterByRestart", func(t *testing.T) {
		restoreOldMasterByRestart(t)
	})

	t.Run("TestRestoreOldMasterInPlace", func(t *testing.T) {
		restoreOldMasterInPlace(t)
	})

	t.Run("TestTerminatedRestore", func(t *testing.T) {
		terminatedRestore(t)
	})

}

type restoreMethod func(t *testing.T, tablet *cluster.Vttablet)

//- create a shard with master and replica1 only
//- run InitShardMaster
//- insert some data
//- take a backup on master
//- insert more data on the master
//- bring up tablet_replica2 after the fact, let it restore the backup
//- check all data is right (before+after backup data)
//- list the backup, remove it
func masterBackup(t *testing.T) {
	verifyInitialReplication(t)

	output, err := localCluster.VtctlclientProcess.ExecuteCommandWithOutput("Backup", master.Alias)
	assert.NotNil(t, err)
	assert.Contains(t, output, "type MASTER cannot take backup. if you really need to do this, rerun the backup command with -allow_master")

	backups := listBackups(t)
	assert.Equal(t, len(backups), 0)

	err = localCluster.VtctlclientProcess.ExecuteCommand("Backup", "-allow_master=true", master.Alias)
	assert.Nil(t, err)

	backups = listBackups(t)
	assert.Equal(t, len(backups), 1)
	assert.Contains(t, backups[0], master.Alias)

	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test2')", keyspaceName, true)
	assert.Nil(t, err)

	restoreWaitForBackup(t, "replica")
	err = replica2.VttabletProcess.WaitForTabletTypesForTimeout([]string{"SERVING"}, 25*time.Second)
	assert.Nil(t, err)

	cluster.VerifyRowsInTablet(t, replica2, keyspaceName, 2)
	cluster.VerifyLocalMetadata(t, replica2, keyspaceName, shardName, cell)
	verifyAfterRemovingBackupNoBackupShouldBePresent(t, backups)

	err = replica2.VttabletProcess.TearDown()
	assert.Nil(t, err)

	_, err = master.VttabletProcess.QueryTablet("DROP TABLE vt_insert_test", keyspaceName, true)
	assert.Nil(t, err)
}

//    Test a master and replica from the same backup.
//
//    Check that a replica and master both restored from the same backup
//    can replicate successfully.
func masterReplicaSameBackup(t *testing.T) {
	// insert data on master, wait for replica to get it
	verifyInitialReplication(t)

	// backup the replica
	err := localCluster.VtctlclientProcess.ExecuteCommand("Backup", replica1.Alias)
	assert.Nil(t, err)

	//  insert more data on the master
	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test2')", keyspaceName, true)
	assert.Nil(t, err)

	// now bring up the other replica, letting it restore from backup.
	restoreWaitForBackup(t, "replica")
	err = replica2.VttabletProcess.WaitForTabletTypesForTimeout([]string{"SERVING"}, 25*time.Second)
	assert.Nil(t, err)

	// check the new replica has the data
	cluster.VerifyRowsInTablet(t, replica2, keyspaceName, 2)

	// Promote replica2 to master
	err = localCluster.VtctlclientProcess.ExecuteCommand("PlannedReparentShard",
		"-keyspace_shard", shardKsName,
		"-new_master", replica2.Alias)
	assert.Nil(t, err)

	// insert more data on replica2 (current master)
	_, err = replica2.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test3')", keyspaceName, true)
	assert.Nil(t, err)

	// Force replica1 to restore from backup.
	verifyRestoreTablet(t, replica1, "SERVING")

	// wait for replica1 to catch up.
	cluster.VerifyRowsInTablet(t, replica1, keyspaceName, 3)

	// This is to test that replicationPosition is processed correctly
	// while doing backup/restore after a reparent.
	// It is written into the MANIFEST and read back from the MANIFEST.
	//
	// Take another backup on the replica.
	err = localCluster.VtctlclientProcess.ExecuteCommand("Backup", replica1.Alias)
	assert.Nil(t, err)

	// Insert more data on replica2 (current master).
	_, err = replica2.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test4')", keyspaceName, true)
	assert.Nil(t, err)

	// Force replica1 to restore from backup.
	verifyRestoreTablet(t, replica1, "SERVING")

	cluster.VerifyRowsInTablet(t, replica1, keyspaceName, 4)
	err = replica2.VttabletProcess.TearDown()
	assert.Nil(t, err)
	restartMasterReplica(t)
}

func restoreOldMasterByRestart(t *testing.T) {
	testRestoreOldMaster(t, restoreUsingRestart)
}

func restoreOldMasterInPlace(t *testing.T) {
	testRestoreOldMaster(t, restoreInPlace)
}

//Test that a former master replicates correctly after being restored.
//
//- Take a backup.
//- Reparent from old master to new master.
//- Force old master to restore from a previous backup using restore_method.
//
//Args:
//restore_method: function accepting one parameter of type tablet.Tablet,
//this function is called to force a restore on the provided tablet
//
func testRestoreOldMaster(t *testing.T, method restoreMethod) {
	// insert data on master, wait for replica to get it
	verifyInitialReplication(t)

	// backup the replica
	err := localCluster.VtctlclientProcess.ExecuteCommand("Backup", replica1.Alias)
	assert.Nil(t, err)

	//  insert more data on the master
	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test2')", keyspaceName, true)
	assert.Nil(t, err)

	// reparent to replica1
	err = localCluster.VtctlclientProcess.ExecuteCommand("PlannedReparentShard",
		"-keyspace_shard", shardKsName,
		"-new_master", replica1.Alias)
	assert.Nil(t, err)

	// insert more data to new master
	_, err = replica1.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test3')", keyspaceName, true)
	assert.Nil(t, err)

	// force the old master to restore at the latest backup.
	method(t, master)

	// wait for it to catch up.
	cluster.VerifyRowsInTablet(t, master, keyspaceName, 3)

	// teardown
	restartMasterReplica(t)
}

func restoreUsingRestart(t *testing.T, tablet *cluster.Vttablet) {
	err := tablet.VttabletProcess.TearDown()
	assert.Nil(t, err)
	verifyRestoreTablet(t, tablet, "SERVING")
}

func restoreInPlace(t *testing.T, tablet *cluster.Vttablet) {
	err := localCluster.VtctlclientProcess.ExecuteCommand("RestoreFromBackup", tablet.Alias)
	assert.Nil(t, err)
}

func restartMasterReplica(t *testing.T) {
	// Stop all master, replica tablet and mysql instance
	stopAllTablets()

	// remove all backups
	backups := listBackups(t)
	for _, backup := range backups {
		err := localCluster.VtctlclientProcess.ExecuteCommand("RemoveBackup", shardKsName, backup)
		assert.Nil(t, err)
	}
	// start all tablet and mysql instances
	var mysqlProcs []*exec.Cmd
	for _, tablet := range []*cluster.Vttablet{master, replica1} {
		proc, _ := tablet.MysqlctlProcess.StartProcess()
		mysqlProcs = append(mysqlProcs, proc)
	}
	for _, proc := range mysqlProcs {
		proc.Wait()
	}
	for _, tablet := range []*cluster.Vttablet{master, replica1} {
		err := localCluster.VtctlclientProcess.InitTablet(tablet, cell, keyspaceName, hostname, shardName)
		assert.Nil(t, err)
		err = tablet.VttabletProcess.CreateDB(keyspaceName)
		assert.Nil(t, err)
		err = tablet.VttabletProcess.Setup()
		assert.Nil(t, err)
	}
	err := localCluster.VtctlclientProcess.InitShardMaster(keyspaceName, shardName, cell, master.TabletUID)
	assert.Nil(t, err)
}

func stopAllTablets() {
	var mysqlProcs []*exec.Cmd
	for _, tablet := range []*cluster.Vttablet{master, replica1, replica2} {
		tablet.VttabletProcess.TearDown()
		proc, _ := tablet.MysqlctlProcess.StopProcess()
		mysqlProcs = append(mysqlProcs, proc)
		localCluster.VtctlclientProcess.ExecuteCommand("DeleteTablet", "-allow_master", tablet.Alias)
	}
	for _, proc := range mysqlProcs {
		proc.Wait()
	}
	for _, tablet := range []*cluster.Vttablet{master, replica1} {
		os.RemoveAll(tablet.VttabletProcess.Directory)
	}
}

func terminatedRestore(t *testing.T) {
	// insert data on master, wait for replica to get it
	verifyInitialReplication(t)

	// backup the replica
	err := localCluster.VtctlclientProcess.ExecuteCommand("Backup", replica1.Alias)
	assert.Nil(t, err)

	//  insert more data on the master
	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test2')", keyspaceName, true)
	assert.Nil(t, err)

	// reparent to replica1
	err = localCluster.VtctlclientProcess.ExecuteCommand("PlannedReparentShard",
		"-keyspace_shard", shardKsName,
		"-new_master", replica1.Alias)
	assert.Nil(t, err)

	// insert more data to new master
	_, err = replica1.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test3')", keyspaceName, true)
	assert.Nil(t, err)

	terminateRestore(t)

	err = localCluster.VtctlclientProcess.ExecuteCommand("RestoreFromBackup", master.Alias)
	assert.Nil(t, err)

	output, err := localCluster.VtctlclientProcess.ExecuteCommandWithOutput("GetTablet", master.Alias)
	assert.Nil(t, err)

	var tabletPB topodata.Tablet
	err = json.Unmarshal([]byte(output), &tabletPB)
	assert.Nil(t, err)
	assert.Equal(t, tabletPB.Type, topodata.TabletType_REPLICA)

	_, err = os.Stat(path.Join(master.VttabletProcess.Directory, "restore_in_progress"))
	assert.True(t, os.IsNotExist(err))

	cluster.VerifyRowsInTablet(t, master, keyspaceName, 3)
	stopAllTablets()
}

//test_backup will:
//- create a shard with master and replica1 only
//- run InitShardMaster
//- bring up tablet_replica2 concurrently, telling it to wait for a backup
//- insert some data
//- take a backup
//- insert more data on the master
//- wait for tablet_replica2 to become SERVING
//- check all data is right (before+after backup data)
//- list the backup, remove it
//
//Args:
//tablet_type: 'replica' or 'rdonly'.
//
//
func vtctlBackup(t *testing.T, tabletType string) {
	restoreWaitForBackup(t, tabletType)
	verifyInitialReplication(t)

	err := localCluster.VtctlclientProcess.ExecuteCommand("Backup", replica1.Alias)
	assert.Nil(t, err)

	backups := listBackups(t)
	assert.Equal(t, len(backups), 1)

	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test2')", keyspaceName, true)
	assert.Nil(t, err)

	err = replica2.VttabletProcess.WaitForTabletTypesForTimeout([]string{"SERVING"}, 25*time.Second)
	assert.Nil(t, err)
	cluster.VerifyRowsInTablet(t, replica2, keyspaceName, 2)

	cluster.VerifyLocalMetadata(t, replica2, keyspaceName, shardName, cell)
	verifyAfterRemovingBackupNoBackupShouldBePresent(t, backups)

	err = replica2.VttabletProcess.TearDown()
	assert.Nil(t, err)

	err = localCluster.VtctlclientProcess.ExecuteCommand("DeleteTablet", replica2.Alias)
	assert.Nil(t, err)
	_, err = master.VttabletProcess.QueryTablet("DROP TABLE vt_insert_test", keyspaceName, true)
	assert.Nil(t, err)

}

// This will create schema in master, insert some data to master and verify the same data in replica
func verifyInitialReplication(t *testing.T) {
	_, err := master.VttabletProcess.QueryTablet(vtInsertTest, keyspaceName, true)
	assert.Nil(t, err)
	_, err = master.VttabletProcess.QueryTablet("insert into vt_insert_test (msg) values ('test1')", keyspaceName, true)
	assert.Nil(t, err)
	cluster.VerifyRowsInTablet(t, replica1, keyspaceName, 1)
}

// Bring up another replica concurrently, telling it to wait until a backup
// is available instead of starting up empty.
//
// Override the backup engine implementation to a non-existent one for restore.
// This setting should only matter for taking new backups. We should be able
// to restore a previous backup successfully regardless of this setting.
func restoreWaitForBackup(t *testing.T, tabletType string) {
	replica2.Type = tabletType
	resetTabletDir(t, replica2)
	replicaTabletArgs := commonTabletArg
	replicaTabletArgs = append(replicaTabletArgs, "-backup_engine_implementation", "fake_implementation")
	replicaTabletArgs = append(replicaTabletArgs, "-wait_for_backup_interval", "1s")
	replicaTabletArgs = append(replicaTabletArgs, "-init_tablet_type", tabletType)
	replica2.VttabletProcess.ExtraArgs = replicaTabletArgs
	replica2.VttabletProcess.ServingStatus = ""
	err := replica2.VttabletProcess.Setup()
	assert.Nil(t, err)
}

func resetTabletDir(t *testing.T, tablet *cluster.Vttablet) {
	err := cluster.ResetTabletDirectory(*tablet)
	assert.Nil(t, err)
}

func listBackups(t *testing.T) []string {
	output, err := localCluster.ListBackups(shardKsName)
	assert.Nil(t, err)
	return output
}

func verifyAfterRemovingBackupNoBackupShouldBePresent(t *testing.T, backups []string) {
	// Remove the backup
	for _, backup := range backups {
		err := localCluster.VtctlclientProcess.ExecuteCommand("RemoveBackup", shardKsName, backup)
		assert.Nil(t, err)
	}

	// Now, there should not be no backup
	backups = listBackups(t)
	assert.Equal(t, len(backups), 0)
}

func verifyRestoreTablet(t *testing.T, tablet *cluster.Vttablet, status string) {
	err := tablet.VttabletProcess.TearDown()
	assert.Nil(t, err)

	resetTabletDir(t, tablet)
	tablet.VttabletProcess.ServingStatus = ""
	err = tablet.VttabletProcess.Setup()
	assert.Nil(t, err)
	if status != "" {
		err = tablet.VttabletProcess.WaitForTabletTypesForTimeout([]string{status}, 25*time.Second)
		assert.Nil(t, err)
	}

	if tablet.Type == "replica" {
		verifyReplicationStatus(t, tablet, "ON")
	} else if tablet.Type == "rdonly" {
		verifyReplicationStatus(t, tablet, "OFF")
	}
}

func verifyReplicationStatus(t *testing.T, vttablet *cluster.Vttablet, expectedStatus string) {
	status, err := vttablet.VttabletProcess.GetDBVar("rpl_semi_sync_slave_enabled", keyspaceName)
	assert.Nil(t, err)
	assert.Equal(t, status, expectedStatus)
	status, err = vttablet.VttabletProcess.GetDBStatus("rpl_semi_sync_slave_status", keyspaceName)
	assert.Nil(t, err)
	assert.Equal(t, status, expectedStatus)
}

func terminateRestore(t *testing.T) {
	stopRestoreMsg := "Copying file 10"
	if useXtrabackup {
		stopRestoreMsg = "Restore: Preparing"
	}

	args := append([]string{"-server", localCluster.VtctlclientProcess.Server, "-alsologtostderr"}, "RestoreFromBackup", master.Alias)
	tmpProcess := exec.Command(
		"vtctlclient",
		args...,
	)

	reader, _ := tmpProcess.StderrPipe()
	err := tmpProcess.Start()
	assert.Nil(t, err)
	found := false

	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		text := scanner.Text()
		if strings.Contains(text, stopRestoreMsg) {
			if _, err := os.Stat(path.Join(master.VttabletProcess.Directory, "restore_in_progress")); os.IsNotExist(err) {
				assert.Fail(t, "restore in progress file missing")
			}
			tmpProcess.Process.Signal(syscall.SIGTERM)
			found = true
			return
		}
	}
	assert.True(t, found, "Restore message not found")
}
