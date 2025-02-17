// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package jobsprotectedts_test

import (
	"context"
	"io"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobsprotectedts"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts/ptpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/scheduledjobs"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/types"
	"github.com/stretchr/testify/require"
)

// TestJobsProtectedTimestamp is an end-to-end test of protected timestamp
// reconciliation for jobs.
func TestJobsProtectedTimestamp(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{})
	defer tc.Stopper().Stop(ctx)

	// Now I want to create some artifacts that should get reconciled away and
	// then make sure that they do and others which should not do not.
	s0 := tc.Server(0)
	ptp := s0.ExecutorConfig().(sql.ExecutorConfig).ProtectedTimestampProvider
	runner := sqlutils.MakeSQLRunner(tc.ServerConn(0))
	runner.Exec(t, "SET CLUSTER SETTING kv.protectedts.reconciliation.interval = '1ms';")
	jr := s0.JobRegistry().(*jobs.Registry)
	mkJobRec := func() jobs.Record {
		return jobs.Record{
			Description: "testing",
			Statements:  []string{"SELECT 1"},
			Username:    security.RootUserName(),
			Details: jobspb.SchemaChangeGCDetails{
				Tables: []jobspb.SchemaChangeGCDetails_DroppedID{
					{
						ID:       42,
						DropTime: s0.Clock().PhysicalNow(),
					},
				},
			},
			Progress:      jobspb.SchemaChangeGCProgress{},
			DescriptorIDs: []descpb.ID{42},
		}
	}
	mkJobAndRecord := func() (j *jobs.Job, rec *ptpb.Record) {
		ts := s0.Clock().Now()
		jobID := jr.MakeJobID()
		require.NoError(t, s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) (err error) {
			if j, err = jr.CreateJobWithTxn(ctx, mkJobRec(), jobID, txn); err != nil {
				return err
			}
			rec = jobsprotectedts.MakeRecord(uuid.MakeV4(), int64(jobID), ts,
				[]roachpb.Span{{Key: keys.MinKey, EndKey: keys.MaxKey}}, jobsprotectedts.Jobs)
			return ptp.Protect(ctx, txn, rec)
		}))
		return j, rec
	}
	jMovedToFailed, recMovedToFailed := mkJobAndRecord()
	require.NoError(t, s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		return jr.Failed(ctx, txn, jMovedToFailed.ID(), io.ErrUnexpectedEOF)
	}))
	jFinished, recFinished := mkJobAndRecord()
	require.NoError(t, s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		return jr.Succeeded(ctx, txn, jFinished.ID())
	}))
	_, recRemains := mkJobAndRecord()
	ensureNotExists := func(ctx context.Context, txn *kv.Txn, ptsID uuid.UUID) (err error) {
		_, err = ptp.GetRecord(ctx, txn, ptsID)
		if err == nil {
			return errors.New("found pts record, waiting for ErrNotExists")
		}
		if errors.Is(err, protectedts.ErrNotExists) {
			return nil
		}
		return errors.Wrap(err, "waiting for ErrNotExists")
	}
	testutils.SucceedsSoon(t, func() (err error) {
		return s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			if err := ensureNotExists(ctx, txn, recMovedToFailed.ID.GetUUID()); err != nil {
				return err
			}
			if err := ensureNotExists(ctx, txn, recFinished.ID.GetUUID()); err != nil {
				return err
			}
			_, err := ptp.GetRecord(ctx, txn, recRemains.ID.GetUUID())
			require.NoError(t, err)
			return err
		})
	})

	// Verify that the two jobs we just observed as removed were recorded in the
	// metrics.
	var removed int
	runner.QueryRow(t, `
SELECT
    value
FROM
    crdb_internal.node_metrics
WHERE
    name = 'kv.protectedts.reconciliation.records_removed';
`).Scan(&removed)
	require.Equal(t, 2, removed)
}

// TestSchedulesProtectedTimestamp is an end-to-end test of protected timestamp
// reconciliation for schedules.
func TestSchedulesProtectedTimestamp(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{})
	defer tc.Stopper().Stop(ctx)

	// Now I want to create some artifacts that should get reconciled away and
	// then make sure that they do and others which should not do not.
	s0 := tc.Server(0)
	ptp := s0.ExecutorConfig().(sql.ExecutorConfig).ProtectedTimestampProvider
	runner := sqlutils.MakeSQLRunner(tc.ServerConn(0))
	runner.Exec(t, "SET CLUSTER SETTING kv.protectedts.reconciliation.interval = '1ms';")
	mkScheduledJobRec := func(scheduleLabel string) *jobs.ScheduledJob {
		j := jobs.NewScheduledJob(scheduledjobs.ProdJobSchedulerEnv)
		j.SetScheduleLabel(scheduleLabel)
		j.SetOwner(security.TestUserName())
		any, err := types.MarshalAny(&jobspb.SqlStatementExecutionArg{Statement: ""})
		require.NoError(t, err)
		j.SetExecutionDetails(jobs.InlineExecutorName, jobspb.ExecutionArguments{Args: any})
		return j
	}
	mkScheduleAndRecord := func(scheduleLabel string) (*jobs.ScheduledJob, *ptpb.Record) {
		ts := s0.Clock().Now()
		var rec *ptpb.Record
		var sj *jobs.ScheduledJob
		require.NoError(t, s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) (err error) {
			sj = mkScheduledJobRec(scheduleLabel)
			require.NoError(t, sj.Create(ctx, s0.InternalExecutor().(sqlutil.InternalExecutor), txn))
			rec = jobsprotectedts.MakeRecord(uuid.MakeV4(), sj.ScheduleID(), ts,
				[]roachpb.Span{{Key: keys.MinKey, EndKey: keys.MaxKey}}, jobsprotectedts.Schedules)
			return ptp.Protect(ctx, txn, rec)
		}))
		return sj, rec
	}
	sjDropped, recScheduleDropped := mkScheduleAndRecord("drop")
	_, err := s0.InternalExecutor().(sqlutil.InternalExecutor).Exec(ctx, "drop-schedule", nil,
		`DROP SCHEDULE $1`, sjDropped.ScheduleID())
	require.NoError(t, err)
	_, recSchedule := mkScheduleAndRecord("do-not-drop")
	ensureNotExists := func(ctx context.Context, txn *kv.Txn, ptsID uuid.UUID) (err error) {
		_, err = ptp.GetRecord(ctx, txn, ptsID)
		if err == nil {
			return errors.New("found pts record, waiting for ErrNotExists")
		}
		if errors.Is(err, protectedts.ErrNotExists) {
			return nil
		}
		return errors.Wrap(err, "waiting for ErrNotExists")
	}
	testutils.SucceedsSoon(t, func() (err error) {
		return s0.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			if err := ensureNotExists(ctx, txn, recScheduleDropped.ID.GetUUID()); err != nil {
				return err
			}
			_, err := ptp.GetRecord(ctx, txn, recSchedule.ID.GetUUID())
			require.NoError(t, err)
			return err
		})
	})

	// Verify that the two jobs we just observed as removed were recorded in the
	// metrics.
	var removed int
	runner.QueryRow(t, `
SELECT
    value
FROM
    crdb_internal.node_metrics
WHERE
    name = 'kv.protectedts.reconciliation.records_removed';
`).Scan(&removed)
	require.Equal(t, 1, removed)
}
