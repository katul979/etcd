// Copyright 2022 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package robustness

import (
	"context"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/model"
	"go.etcd.io/etcd/tests/v3/robustness/traffic"
)

func TestRobustness(t *testing.T) {
	testRunner.BeforeTest(t)
	v, err := e2e.GetVersionFromBinary(e2e.BinPath.Etcd)
	if err != nil {
		t.Fatalf("Failed checking etcd version binary, binary: %q, err: %v", e2e.BinPath.Etcd, err)
	}
	scenarios := []testScenario{}
	for _, traffic := range []traffic.Config{traffic.LowTraffic, traffic.HighTraffic, traffic.KubernetesTraffic} {
		scenarios = append(scenarios, testScenario{
			name:      "ClusterOfSize1/" + traffic.Name,
			failpoint: RandomFailpoint,
			traffic:   traffic,
			cluster: *e2e.NewConfig(
				e2e.WithClusterSize(1),
				e2e.WithSnapshotCount(100),
				e2e.WithGoFailEnabled(true),
				e2e.WithCompactionBatchLimit(100), // required for compactBeforeCommitBatch and compactAfterCommitBatch failpoints
				e2e.WithWatchProcessNotifyInterval(100*time.Millisecond),
			),
		})
		clusterOfSize3Options := []e2e.EPClusterOption{
			e2e.WithIsPeerTLS(true),
			e2e.WithSnapshotCount(100),
			e2e.WithPeerProxy(true),
			e2e.WithGoFailEnabled(true),
			e2e.WithCompactionBatchLimit(100), // required for compactBeforeCommitBatch and compactAfterCommitBatch failpoints
			e2e.WithWatchProcessNotifyInterval(100 * time.Millisecond),
		}
		if !v.LessThan(version.V3_6) {
			clusterOfSize3Options = append(clusterOfSize3Options, e2e.WithSnapshotCatchUpEntries(100))
		}
		scenarios = append(scenarios, testScenario{
			name:      "ClusterOfSize3/" + traffic.Name,
			failpoint: RandomFailpoint,
			traffic:   traffic,
			cluster:   *e2e.NewConfig(clusterOfSize3Options...),
		})
	}
	scenarios = append(scenarios, testScenario{
		name:      "Issue14370",
		failpoint: RaftBeforeSavePanic,
		cluster: *e2e.NewConfig(
			e2e.WithClusterSize(1),
			e2e.WithGoFailEnabled(true),
		),
	})
	scenarios = append(scenarios, testScenario{
		name:      "Issue14685",
		failpoint: DefragBeforeCopyPanic,
		cluster: *e2e.NewConfig(
			e2e.WithClusterSize(1),
			e2e.WithGoFailEnabled(true),
		),
	})
	scenarios = append(scenarios, testScenario{
		name:      "Issue13766",
		failpoint: KillFailpoint,
		traffic:   traffic.HighTraffic,
		cluster: *e2e.NewConfig(
			e2e.WithSnapshotCount(100),
		),
	})
	scenarios = append(scenarios, testScenario{
		name:      "Issue15220",
		failpoint: RandomFailpoint,
		watch: watchConfig{
			requestProgress: true,
		},
		cluster: *e2e.NewConfig(
			e2e.WithClusterSize(1),
		),
	})
	if v.Compare(version.V3_5) >= 0 {
		scenarios = append(scenarios, testScenario{
			name:      "Issue15271",
			failpoint: BlackholeUntilSnapshot,
			traffic:   traffic.HighTraffic,
			cluster: *e2e.NewConfig(
				e2e.WithSnapshotCount(100),
				e2e.WithPeerProxy(true),
				e2e.WithIsPeerTLS(true),
			),
		})
	}
	for _, scenario := range scenarios {
		if scenario.traffic == (traffic.Config{}) {
			scenario.traffic = traffic.LowTraffic
		}

		t.Run(scenario.name, func(t *testing.T) {
			lg := zaptest.NewLogger(t)
			scenario.cluster.Logger = lg
			ctx := context.Background()
			testRobustness(ctx, t, lg, scenario)
		})
	}
}

type testScenario struct {
	name      string
	failpoint Failpoint
	cluster   e2e.EtcdProcessClusterConfig
	traffic   traffic.Config
	watch     watchConfig
}

func testRobustness(ctx context.Context, t *testing.T, lg *zap.Logger, s testScenario) {
	r := report{lg: lg}
	var err error
	r.clus, err = e2e.NewEtcdProcessCluster(ctx, t, e2e.WithConfig(&s.cluster))
	if err != nil {
		t.Fatal(err)
	}
	defer r.clus.Close()

	// t.Failed() returns false during panicking. We need to forcibly
	// save data on panicking.
	// Refer to: https://github.com/golang/go/issues/49929
	panicked := true
	defer func() {
		r.Report(t, panicked)
	}()
	r.operations, r.responses = s.run(ctx, t, lg, r.clus)
	forcestopCluster(r.clus)

	watchProgressNotifyEnabled := r.clus.Cfg.WatchProcessNotifyInterval != 0
	validateWatchResponses(t, r.clus, r.responses, s.watch.requestProgress || watchProgressNotifyEnabled)

	r.events = watchEvents(r.responses)
	validateEventsMatch(t, r.events)

	r.patchedOperations = patchOperationBasedOnWatchEvents(r.operations, longestHistory(r.events))
	r.visualizeHistory = model.ValidateOperationHistoryAndReturnVisualize(t, lg, r.patchedOperations)

	panicked = false
}

func (s testScenario) run(ctx context.Context, t *testing.T, lg *zap.Logger, clus *e2e.EtcdProcessCluster) (operations []porcupine.Operation, responses [][]traffic.WatchResponse) {
	g := errgroup.Group{}
	finishTraffic := make(chan struct{})

	// baseTime is used to get monotonic clock reading when recording operations/watch events
	baseTime := time.Now()
	g.Go(func() error {
		defer close(finishTraffic)
		injectFailpoints(ctx, t, lg, clus, s.failpoint)
		time.Sleep(time.Second)
		return nil
	})
	maxRevisionChan := make(chan int64, 1)
	g.Go(func() error {
		defer close(maxRevisionChan)
		operations = traffic.SimulateTraffic(ctx, t, lg, clus, s.traffic, finishTraffic, baseTime)
		maxRevisionChan <- operationsMaxRevision(operations)
		return nil
	})
	g.Go(func() error {
		responses = collectClusterWatchEvents(ctx, t, clus, maxRevisionChan, s.watch, baseTime)
		return nil
	})
	g.Wait()
	return operations, responses
}

func operationsMaxRevision(operations []porcupine.Operation) int64 {
	var maxRevision int64
	for _, op := range operations {
		revision := op.Output.(model.EtcdNonDeterministicResponse).Revision
		if revision > maxRevision {
			maxRevision = revision
		}
	}
	return maxRevision
}

// forcestopCluster stops the etcd member with signal kill.
func forcestopCluster(clus *e2e.EtcdProcessCluster) error {
	for _, member := range clus.Procs {
		member.Kill()
	}
	return clus.ConcurrentStop()
}
