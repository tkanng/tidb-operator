// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.package spec

package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/pingcap/tidb-operator/tests/slack"

	"github.com/golang/glog"
	"github.com/jinzhu/copier"
	"github.com/pingcap/tidb-operator/tests"
	"github.com/pingcap/tidb-operator/tests/pkg/client"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/util/logs"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()
	go func() {
		glog.Info(http.ListenAndServe(":6060", nil))
	}()

	conf := tests.ParseConfigOrDie()
	cli, kubeCli := client.NewCliOrDie()
	fta := tests.NewFaultTriggerAction(cli, kubeCli, conf)
	fta.CheckAndRecoverEnvOrDie()

	tidbVersion := conf.GetTiDBVersionOrDie()
	upgardeTiDBVersions := conf.GetUpgradeTidbVersionsOrDie()

	// operator config
	operatorCfg := &tests.OperatorConfig{
		Namespace:          "pingcap",
		ReleaseName:        "operator",
		Image:              conf.OperatorImage,
		Tag:                conf.OperatorTag,
		SchedulerImage:     "gcr.io/google-containers/hyperkube",
		LogLevel:           "2",
		WebhookServiceName: "webhook-service",
		WebhookSecretName:  "webhook-secret",
		WebhookConfigName:  "webhook-config",
	}

	// TODO remove this
	// create database and table and insert a column for test backup and restore
	initSQL := `"create database record;use record;create table test(t char(32))"`

	// two clusters in different namespaces
	clusterName1 := "stability-cluster1"
	clusterName2 := "stability-cluster2"
	cluster1 := &tests.TidbClusterConfig{
		Namespace:        clusterName1,
		ClusterName:      clusterName1,
		OperatorTag:      conf.OperatorTag,
		PDImage:          fmt.Sprintf("pingcap/pd:%s", tidbVersion),
		TiKVImage:        fmt.Sprintf("pingcap/tikv:%s", tidbVersion),
		TiDBImage:        fmt.Sprintf("pingcap/tidb:%s", tidbVersion),
		StorageClassName: "local-storage",
		Password:         "admin",
		InitSQL:          initSQL,
		UserName:         "root",
		InitSecretName:   fmt.Sprintf("%s-set-secret", clusterName1),
		BackupSecretName: fmt.Sprintf("%s-backup-secret", clusterName1),
		BackupName:       "backup",
		Resources: map[string]string{
			"pd.resources.limits.cpu":        "1000m",
			"pd.resources.limits.memory":     "2Gi",
			"pd.resources.requests.cpu":      "200m",
			"pd.resources.requests.memory":   "1Gi",
			"tikv.resources.limits.cpu":      "8000m",
			"tikv.resources.limits.memory":   "8Gi",
			"tikv.resources.requests.cpu":    "1000m",
			"tikv.resources.requests.memory": "2Gi",
			"tidb.resources.limits.cpu":      "8000m",
			"tidb.resources.limits.memory":   "8Gi",
			"tidb.resources.requests.cpu":    "500m",
			"tidb.resources.requests.memory": "1Gi",
			"monitor.persistent":             "true",
			"discovery.image":                conf.OperatorImage,
		},
		Args: map[string]string{
			"binlog.drainer.workerCount": "1024",
			"binlog.drainer.txnBatch":    "512",
		},
		Monitor:          true,
		BlockWriteConfig: conf.BlockWriter,
	}
	cluster2 := &tests.TidbClusterConfig{
		Namespace:        clusterName2,
		ClusterName:      clusterName2,
		OperatorTag:      conf.OperatorTag,
		PDImage:          fmt.Sprintf("pingcap/pd:%s", tidbVersion),
		TiKVImage:        fmt.Sprintf("pingcap/tikv:%s", tidbVersion),
		TiDBImage:        fmt.Sprintf("pingcap/tidb:%s", tidbVersion),
		StorageClassName: "local-storage",
		Password:         "admin",
		InitSQL:          initSQL,
		UserName:         "root",
		InitSecretName:   fmt.Sprintf("%s-set-secret", clusterName2),
		BackupSecretName: fmt.Sprintf("%s-backup-secret", clusterName2),
		BackupName:       "backup",
		Resources: map[string]string{
			"pd.resources.limits.cpu":        "1000m",
			"pd.resources.limits.memory":     "2Gi",
			"pd.resources.requests.cpu":      "200m",
			"pd.resources.requests.memory":   "1Gi",
			"tikv.resources.limits.cpu":      "8000m",
			"tikv.resources.limits.memory":   "8Gi",
			"tikv.resources.requests.cpu":    "1000m",
			"tikv.resources.requests.memory": "2Gi",
			"tidb.resources.limits.cpu":      "8000m",
			"tidb.resources.limits.memory":   "8Gi",
			"tidb.resources.requests.cpu":    "500m",
			"tidb.resources.requests.memory": "1Gi",
			// TODO assert the the monitor's pvc exist and clean it when bootstrapping
			"monitor.persistent": "true",
			"discovery.image":    conf.OperatorImage,
		},
		Args:             map[string]string{},
		Monitor:          true,
		BlockWriteConfig: conf.BlockWriter,
	}

	// cluster backup and restore
	clusterBackupFrom := cluster1
	clusterRestoreTo := &tests.TidbClusterConfig{}
	copier.Copy(clusterRestoreTo, clusterBackupFrom)
	clusterRestoreTo.ClusterName = "cluster-restore"

	allClusters := []*tests.TidbClusterConfig{cluster1, cluster2, clusterRestoreTo}

	oa := tests.NewOperatorActions(cli, kubeCli, tests.DefaultPollInterval, conf, allClusters)
	oa.CheckK8sAvailableOrDie(nil, nil)
	go wait.Forever(oa.EventWorker, 10*time.Second)
	// start a http server in goruntine
	go oa.StartValidatingAdmissionWebhookServerOrDie(operatorCfg)

	defer func() {
		oa.DumpAllLogs(operatorCfg, allClusters)
	}()

	// clean and deploy operator
	oa.CleanOperatorOrDie(operatorCfg)
	oa.DeployOperatorOrDie(operatorCfg)

	// clean all clusters
	for _, cluster := range allClusters {
		oa.CleanTidbClusterOrDie(cluster)
	}

	// deploy and check cluster1, cluster2
	oa.DeployTidbClusterOrDie(cluster1)
	oa.DeployTidbClusterOrDie(cluster2)
	oa.CheckTidbClusterStatusOrDie(cluster1)
	oa.CheckTidbClusterStatusOrDie(cluster2)

	go oa.BeginInsertDataToOrDie(cluster1)
	go oa.BeginInsertDataToOrDie(cluster2)

	// scale out cluster1 and cluster2
	cluster1.ScaleTiDB(3).ScaleTiKV(5).ScalePD(5)
	oa.ScaleTidbClusterOrDie(cluster1)
	cluster2.ScaleTiDB(3).ScaleTiKV(5).ScalePD(5)
	oa.ScaleTidbClusterOrDie(cluster2)
	oa.CheckTidbClusterStatusOrDie(cluster1)
	oa.CheckTidbClusterStatusOrDie(cluster2)

	// scale in cluster1 and cluster2
	cluster1.ScaleTiDB(2).ScaleTiKV(3).ScalePD(3)
	oa.ScaleTidbClusterOrDie(cluster1)
	cluster2.ScaleTiDB(2).ScaleTiKV(3).ScalePD(3)
	oa.ScaleTidbClusterOrDie(cluster2)
	oa.CheckTidbClusterStatusOrDie(cluster1)
	oa.CheckTidbClusterStatusOrDie(cluster2)

	// before upgrade cluster, register webhook first
	oa.RegisterWebHookAndServiceOrDie(operatorCfg)

	// upgrade cluster1 and cluster2
	firstUpgradeVersion := upgardeTiDBVersions[0]
	cluster1.UpgradeAll(firstUpgradeVersion)
	cluster2.UpgradeAll(firstUpgradeVersion)
	oa.UpgradeTidbClusterOrDie(cluster1)
	oa.UpgradeTidbClusterOrDie(cluster2)
	oa.CheckTidbClusterStatusOrDie(cluster1)
	oa.CheckTidbClusterStatusOrDie(cluster2)

	// after upgrade cluster, clean webhook
	oa.CleanWebHookAndService(operatorCfg)

	// deploy and check cluster restore
	oa.DeployTidbClusterOrDie(clusterRestoreTo)
	oa.CheckTidbClusterStatusOrDie(clusterRestoreTo)

	// backup and restore
	oa.BackupRestoreOrDie(clusterBackupFrom, clusterRestoreTo)

	// stop a node and failover automatically
	physicalNode, node, faultTime := fta.StopNodeOrDie()
	oa.EmitEvent(nil, fmt.Sprintf("StopNode: %s on %s", node, physicalNode))
	oa.CheckFailoverPendingOrDie(allClusters, node, &faultTime)
	oa.CheckFailoverOrDie(allClusters, node)
	time.Sleep(3 * time.Minute)
	fta.StartNodeOrDie(physicalNode, node)
	oa.EmitEvent(nil, fmt.Sprintf("StartNode: %s on %s", node, physicalNode))
	oa.CheckRecoverOrDie(allClusters)
	for _, cluster := range allClusters {
		oa.CheckTidbClusterStatusOrDie(cluster)
	}

	// truncate a sst file and check failover
	oa.TruncateSSTFileThenCheckFailoverOrDie(cluster1, 5*time.Minute)

	// stop one etcd node and k8s/operator/tidbcluster is available
	faultEtcd := tests.SelectNode(conf.ETCDs)
	fta.StopETCDOrDie(faultEtcd)
	defer fta.StartETCDOrDie(faultEtcd)
	// TODO make the pause interval as a argument
	time.Sleep(3 * time.Minute)
	oa.CheckOneEtcdDownOrDie(operatorCfg, allClusters, faultEtcd)
	fta.StartETCDOrDie(faultEtcd)

	//clean temp dirs when stability success
	err := conf.CleanTempDirs()
	if err != nil {
		glog.Errorf("failed to clean temp dirs, this error can be ignored.")
	}

	slack.NotifyAndCompleted("\nFinished.")
}
