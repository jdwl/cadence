// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"fmt"
	"time"

	"github.com/uber-common/bark"
	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/errors"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

const (
	secondsInDay = int32(24 * time.Hour / time.Second)
)

type (
	workflowExecutionContext struct {
		domainID          string
		workflowExecution workflow.WorkflowExecution
		shard             ShardContext
		clusterMetadata   cluster.Metadata
		executionManager  persistence.ExecutionManager
		logger            bark.Logger
		metricsClient     metrics.Client

		locker                common.Mutex
		msBuilder             mutableState
		updateCondition       int64
		deleteTimerTask       persistence.Task
		createReplicationTask bool
	}
)

var (
	persistenceOperationRetryPolicy = common.CreatePersistanceRetryPolicy()
)

func newWorkflowExecutionContext(domainID string, execution workflow.WorkflowExecution, shard ShardContext,
	executionManager persistence.ExecutionManager, logger bark.Logger) *workflowExecutionContext {
	lg := logger.WithFields(bark.Fields{
		logging.TagWorkflowExecutionID: *execution.WorkflowId,
		logging.TagWorkflowRunID:       *execution.RunId,
	})

	return &workflowExecutionContext{
		domainID:          domainID,
		workflowExecution: execution,
		shard:             shard,
		clusterMetadata:   shard.GetService().GetClusterMetadata(),
		executionManager:  executionManager,
		logger:            lg,
		metricsClient:     shard.GetMetricsClient(),
		locker:            common.NewMutex(),
	}
}

func (c *workflowExecutionContext) loadWorkflowExecution() (mutableState, error) {
	err := c.loadWorkflowExecutionInternal()
	if err != nil {
		return nil, err
	}
	err = c.updateVersion()
	if err != nil {
		return nil, err
	}
	return c.msBuilder, nil
}

func (c *workflowExecutionContext) loadWorkflowExecutionInternal() error {
	if c.msBuilder != nil {
		return nil
	}

	response, err := c.getWorkflowExecutionWithRetry(&persistence.GetWorkflowExecutionRequest{
		DomainID:  c.domainID,
		Execution: c.workflowExecution,
	})
	if err != nil {
		if common.IsPersistenceTransientError(err) {
			logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationGetWorkflowExecution, err, "")
		}
		return err
	}

	msBuilder := newMutableStateBuilder(c.clusterMetadata.GetCurrentClusterName(), c.shard.GetConfig(), c.logger)
	if response != nil && response.State != nil {
		state := response.State
		msBuilder.Load(state)
		info := state.ExecutionInfo
		c.updateCondition = info.NextEventID
	}

	c.msBuilder = msBuilder
	c.logger.WithFields(bark.Fields{
		logging.TagDomainID:            c.domainID,
		logging.TagWorkflowExecutionID: c.msBuilder.GetExecutionInfo().WorkflowID,
		logging.TagWorkflowRunID:       c.msBuilder.GetExecutionInfo().RunID,
		"size":                         c.msBuilder.GetHistorySize(),
	}).Info("debug historySize")
	// finally emit execution and session stats
	c.emitWorkflowExecutionStats(response.MutableStateStats, c.msBuilder.GetHistorySize())
	return nil
}

func (c *workflowExecutionContext) resetWorkflowExecution(prevRunID string, resetBuilder mutableState) (mutableState,
	error) {
	snapshotRequest := resetBuilder.ResetSnapshot(prevRunID)
	snapshotRequest.Condition = c.updateCondition

	err := c.shard.ResetMutableState(snapshotRequest)
	if err != nil {
		return nil, err
	}

	c.clear()
	return c.loadWorkflowExecution()
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithContext(context []byte, transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {
	c.msBuilder.GetExecutionInfo().ExecutionContext = context

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithNewRunAndContext(context []byte, transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64, newStateBuilder mutableState) error {
	c.msBuilder.GetExecutionInfo().ExecutionContext = context

	return c.updateWorkflowExecutionWithNewRun(transferTasks, timerTasks, transactionID, newStateBuilder)
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithDeleteTask(transferTasks []persistence.Task,
	timerTasks []persistence.Task, deleteTimerTask persistence.Task, transactionID int64) error {
	c.deleteTimerTask = deleteTimerTask

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) replicateWorkflowExecution(request *h.ReplicateEventsRequest,
	transferTasks []persistence.Task, timerTasks []persistence.Task, lastEventID, transactionID int64, now time.Time) error {
	nextEventID := lastEventID + 1
	c.msBuilder.GetExecutionInfo().SetNextEventID(nextEventID)

	standbyHistoryBuilder := newHistoryBuilderFromEvents(request.History.Events, c.logger)
	return c.updateHelper(transferTasks, timerTasks, transactionID, now, false, standbyHistoryBuilder, request.GetSourceCluster())
}

func (c *workflowExecutionContext) updateVersion() error {
	if c.shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled() && c.msBuilder.GetReplicationState() != nil {
		if !c.msBuilder.IsWorkflowExecutionRunning() {
			// we should not update the version on mutable state when the workflow is finished
			return nil
		}
		// Support for global domains is enabled and we are performing an update for global domain
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.domainID)
		if err != nil {
			return err
		}
		c.msBuilder.UpdateReplicationStateVersion(domainEntry.GetFailoverVersion(), false)

		// this is a hack, only create replication task if have # target cluster > 1, for more see #868
		c.createReplicationTask = domainEntry.CanReplicateEvent()
	}
	return nil
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithNewRun(transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64, newStateBuilder mutableState) error {
	if c.msBuilder.GetReplicationState() != nil {
		currentVersion := c.msBuilder.GetCurrentVersion()

		activeCluster := c.clusterMetadata.ClusterNameForFailoverVersion(currentVersion)
		currentCluster := c.clusterMetadata.GetCurrentClusterName()
		if activeCluster != currentCluster {
			domainID := c.msBuilder.GetExecutionInfo().DomainID
			c.clear()
			return errors.NewDomainNotActiveError(domainID, currentCluster, activeCluster)
		}

		// Handling mutable state turn from standby to active, while having a decision on the fly
		if di, ok := c.msBuilder.GetInFlightDecisionTask(); ok && c.msBuilder.IsWorkflowExecutionRunning() {
			if di.Version < currentVersion {
				// we have a decision on the fly with a lower version, fail it
				c.msBuilder.AddDecisionTaskFailedEvent(di.ScheduleID, di.StartedID,
					workflow.DecisionTaskFailedCauseFailoverCloseDecision, nil, identityHistoryService)

				var transT, timerT []persistence.Task
				transT, timerT, err := c.scheduleNewDecision(transT, timerT)
				if err != nil {
					return err
				}
				transferTasks = append(transferTasks, transT...)
				timerTasks = append(timerTasks, timerT...)
			}
		}
	}

	if !c.createReplicationTask {
		c.logger.Debugf("Skipping replication task creation: %v, workflowID: %v, runID: %v, firstEventID: %v, nextEventID: %v.",
			c.domainID, c.workflowExecution.GetWorkflowId(), c.workflowExecution.GetRunId(),
			c.msBuilder.GetExecutionInfo().LastFirstEventID, c.msBuilder.GetExecutionInfo().NextEventID)
	}

	now := time.Now()
	return c.updateHelperWithNewRun(transferTasks, timerTasks, transactionID, now, c.createReplicationTask, nil, "", newStateBuilder)
}

func (c *workflowExecutionContext) updateWorkflowExecution(transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {
	return c.updateWorkflowExecutionWithNewRun(transferTasks, timerTasks, transactionID, nil)
}

func (c *workflowExecutionContext) updateHelper(transferTasks []persistence.Task, timerTasks []persistence.Task,
	transactionID int64, now time.Time,
	createReplicationTask bool, standbyHistoryBuilder *historyBuilder, sourceCluster string) (errRet error) {
	return c.updateHelperWithNewRun(transferTasks, timerTasks, transactionID, now, createReplicationTask, standbyHistoryBuilder, sourceCluster, nil)
}

func (c *workflowExecutionContext) updateHelperWithNewRun(transferTasks []persistence.Task, timerTasks []persistence.Task,
	transactionID int64, now time.Time,
	createReplicationTask bool, standbyHistoryBuilder *historyBuilder, sourceCluster string, newStateBuilder mutableState) (errRet error) {

	defer func() {
		if errRet != nil {
			// Clear all cached state in case of error
			c.clear()
		}
	}()

	// Take a snapshot of all updates we have accumulated for this execution
	updates, err := c.msBuilder.CloseUpdateSession()
	if err != nil {
		return err
	}

	executionInfo := c.msBuilder.GetExecutionInfo()

	// this builder has events generated locally
	hasNewStandbyHistoryEvents := standbyHistoryBuilder != nil && len(standbyHistoryBuilder.history) > 0
	activeHistoryBuilder := updates.newEventsBuilder
	hasNewActiveHistoryEvents := len(activeHistoryBuilder.history) > 0

	if hasNewStandbyHistoryEvents && hasNewActiveHistoryEvents {
		c.logger.WithFields(bark.Fields{
			logging.TagDomainID:            executionInfo.DomainID,
			logging.TagWorkflowExecutionID: executionInfo.WorkflowID,
			logging.TagWorkflowRunID:       executionInfo.RunID,
			logging.TagFirstEventID:        executionInfo.LastFirstEventID,
			logging.TagNextEventID:         executionInfo.NextEventID,
			logging.TagReplicationState:    c.msBuilder.GetReplicationState(),
		}).Fatal("Both standby and active history builder has events.")
	}

	// Replication state should only be updated after the UpdateSession is closed.  IDs for certain events are only
	// generated on CloseSession as they could be buffered events.  The value for NextEventID will be wrong on
	// mutable state if read before flushing the buffered events.
	crossDCEnabled := c.msBuilder.GetReplicationState() != nil
	if crossDCEnabled {
		// always standby history first
		if hasNewStandbyHistoryEvents {
			lastEvent := standbyHistoryBuilder.history[len(standbyHistoryBuilder.history)-1]
			c.msBuilder.UpdateReplicationStateLastEventID(
				sourceCluster,
				lastEvent.GetVersion(),
				lastEvent.GetEventId(),
			)
		}

		if hasNewActiveHistoryEvents {
			c.msBuilder.UpdateReplicationStateLastEventID(
				c.clusterMetadata.GetCurrentClusterName(),
				c.msBuilder.GetCurrentVersion(),
				executionInfo.NextEventID-1,
			)
		}
	}

	historySize := 0
	// always standby history first
	if hasNewStandbyHistoryEvents {
		firstEvent := standbyHistoryBuilder.GetFirstEvent()
		// Note: standby events has no transient decision events
		historySize, err = c.appendHistoryEvents(standbyHistoryBuilder, standbyHistoryBuilder.history, transactionID)
		if err != nil {
			return err
		}

		executionInfo.SetLastFirstEventID(firstEvent.GetEventId())
	}

	// Some operations only update the mutable state. For example RecordActivityTaskHeartbeat.
	if hasNewActiveHistoryEvents {
		firstEvent := activeHistoryBuilder.GetFirstEvent()
		// Transient decision events need to be written as a separate batch
		if activeHistoryBuilder.HasTransientEvents() {
			historySize, err = c.appendHistoryEvents(activeHistoryBuilder, activeHistoryBuilder.transientHistory, transactionID)
			if err != nil {
				return err
			}
		}

		var size int
		size, err = c.appendHistoryEvents(activeHistoryBuilder, activeHistoryBuilder.history, transactionID)
		if err != nil {
			return err
		}

		executionInfo.SetLastFirstEventID(firstEvent.GetEventId())
		historySize += size
	}

	continueAsNew := updates.continueAsNew
	finishExecution := false
	var finishExecutionTTL int32
	if executionInfo.State == persistence.WorkflowStateCompleted {
		// Workflow execution completed as part of this transaction.
		// Also transactionally delete workflow execution representing
		// current run for the execution using cassandra TTL
		finishExecution = true
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(executionInfo.DomainID)
		if err != nil {
			return err
		}
		// NOTE: domain retention is in days, so we need to do a conversion
		finishExecutionTTL = domainEntry.GetRetentionDays(executionInfo.WorkflowID) * secondsInDay

		// clear stickness
		c.msBuilder.ClearStickyness()
	}

	var replicationTasks []persistence.Task
	// Check if the update resulted in new history events before generating replication task
	if createReplicationTask {
		// Let's create a replication task as part of this update
		if hasNewActiveHistoryEvents {
			if newStateBuilder != nil && newStateBuilder.GetEventStoreVersion() == persistence.EventStoreVersionV2 {
				replicationTasks = append(replicationTasks, c.msBuilder.CreateReplicationTask(persistence.EventStoreVersionV2, newStateBuilder.GetCurrentBranch()))
			} else {
				replicationTasks = append(replicationTasks, c.msBuilder.CreateReplicationTask(0, nil))
			}
		}
		if c.shard.GetConfig().EnableSyncActivityHeartbeat() {
			replicationTasks = append(replicationTasks, updates.syncActivityTasks...)
		}
	}

	setTaskInfo(c.msBuilder.GetCurrentVersion(), now, transferTasks, timerTasks)

	// Update history size on mutableState before calling UpdateWorkflowExecution
	c.msBuilder.IncrementHistorySize(historySize)

	var resp *persistence.UpdateWorkflowExecutionResponse
	var err1 error
	if resp, err1 = c.updateWorkflowExecutionWithRetry(&persistence.UpdateWorkflowExecutionRequest{
		ExecutionInfo:                 executionInfo,
		ReplicationState:              c.msBuilder.GetReplicationState(),
		TransferTasks:                 transferTasks,
		ReplicationTasks:              replicationTasks,
		TimerTasks:                    timerTasks,
		Condition:                     c.updateCondition,
		DeleteTimerTask:               c.deleteTimerTask,
		UpsertActivityInfos:           updates.updateActivityInfos,
		DeleteActivityInfos:           updates.deleteActivityInfos,
		UpserTimerInfos:               updates.updateTimerInfos,
		DeleteTimerInfos:              updates.deleteTimerInfos,
		UpsertChildExecutionInfos:     updates.updateChildExecutionInfos,
		DeleteChildExecutionInfo:      updates.deleteChildExecutionInfo,
		UpsertRequestCancelInfos:      updates.updateCancelExecutionInfos,
		DeleteRequestCancelInfo:       updates.deleteCancelExecutionInfo,
		UpsertSignalInfos:             updates.updateSignalInfos,
		DeleteSignalInfo:              updates.deleteSignalInfo,
		UpsertSignalRequestedIDs:      updates.updateSignalRequestedIDs,
		DeleteSignalRequestedID:       updates.deleteSignalRequestedID,
		NewBufferedEvents:             updates.newBufferedEvents,
		ClearBufferedEvents:           updates.clearBufferedEvents,
		NewBufferedReplicationTask:    updates.newBufferedReplicationEventsInfo,
		DeleteBufferedReplicationTask: updates.deleteBufferedReplicationEvent,
		ContinueAsNew:                 continueAsNew,
		FinishExecution:               finishExecution,
		FinishedExecutionTTL:          finishExecutionTTL,
	}); err1 != nil {
		switch err1.(type) {
		case *persistence.ConditionFailedError:
			return ErrConflict
		}

		logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err1,
			fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
		return err1
	}

	// Update went through so update the condition for new updates
	c.updateCondition = c.msBuilder.GetNextEventID()
	c.msBuilder.GetExecutionInfo().LastUpdatedTimestamp = time.Now()

	// for any change in the workflow, send a event
	c.shard.NotifyNewHistoryEvent(newHistoryEventNotification(
		c.domainID,
		&c.workflowExecution,
		c.msBuilder.GetLastFirstEventID(),
		c.msBuilder.GetNextEventID(),
		c.msBuilder.IsWorkflowExecutionRunning(),
	))

	// finally emit session stats
	if resp != nil {
		c.emitSessionUpdateStats(resp.MutableStateUpdateSessionStats)
	}

	return nil
}

func (c *workflowExecutionContext) appendHistoryEvents(builder *historyBuilder, history []*workflow.HistoryEvent,
	transactionID int64) (int, error) {

	firstEvent := history[0]
	var historySize int
	var err error

	if c.msBuilder.GetEventStoreVersion() == persistence.EventStoreVersionV2 {
		historySize, err = c.shard.AppendHistoryV2Events(&persistence.AppendHistoryNodesRequest{
			IsNewBranch:   false,
			BranchToken:   c.msBuilder.GetCurrentBranch(),
			Events:        history,
			TransactionID: transactionID,
		}, c.domainID)
	} else {
		historySize, err = c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
			DomainID:          c.domainID,
			Execution:         c.workflowExecution,
			TransactionID:     transactionID,
			FirstEventID:      firstEvent.GetEventId(),
			EventBatchVersion: firstEvent.GetVersion(),
			Events:            history,
		})
	}

	if err != nil {
		switch err.(type) {
		case *persistence.ConditionFailedError:
			return historySize, ErrConflict
		}

		logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err,
			fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
		return historySize, err
	}

	return historySize, nil
}

func (c *workflowExecutionContext) replicateContinueAsNewWorkflowExecution(newStateBuilder mutableState,
	transactionID int64) error {
	return c.appendFirstBatchHistoryForContinueAsNew(nil, newStateBuilder, transactionID)
}

func (c *workflowExecutionContext) continueAsNewWorkflowExecution(context []byte, newStateBuilder mutableState,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {

	err1 := c.appendFirstBatchHistoryForContinueAsNew(context, newStateBuilder, transactionID)
	if err1 != nil {
		return err1
	}

	err2 := c.updateWorkflowExecutionWithNewRunAndContext(context, transferTasks, timerTasks, transactionID, newStateBuilder)
	if err2 != nil {
		// TODO: Delete new execution if update fails due to conflict or shard being lost
	}

	return err2
}

func (c *workflowExecutionContext) appendFirstBatchHistoryForContinueAsNew(context []byte, newStateBuilder mutableState,
	transactionID int64) error {
	executionInfo := newStateBuilder.GetExecutionInfo()
	domainID := executionInfo.DomainID
	newExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(executionInfo.WorkflowID),
		RunId:      common.StringPtr(executionInfo.RunID),
	}

	firstEvent := newStateBuilder.GetHistoryBuilder().history[0]
	history := newStateBuilder.GetHistoryBuilder().GetHistory()
	var historySize int
	var err error
	if newStateBuilder.GetEventStoreVersion() == persistence.EventStoreVersionV2 {
		historySize, err = c.shard.AppendHistoryV2Events(&persistence.AppendHistoryNodesRequest{
			IsNewBranch:   true,
			BranchToken:   newStateBuilder.GetCurrentBranch(),
			Events:        history.Events,
			TransactionID: transactionID,
		}, newStateBuilder.GetExecutionInfo().DomainID)
	} else {
		historySize, err = c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
			DomainID:          domainID,
			Execution:         newExecution,
			TransactionID:     transactionID,
			FirstEventID:      firstEvent.GetEventId(),
			EventBatchVersion: firstEvent.GetVersion(),
			Events:            history.Events,
		})
	}

	if err == nil {
		// History update for new run succeeded, update the history size on both mutableState for current and new run
		c.msBuilder.SetNewRunSize(historySize)
		newStateBuilder.IncrementHistorySize(historySize)
	}

	return err
}

func (c *workflowExecutionContext) getWorkflowExecutionWithRetry(
	request *persistence.GetWorkflowExecutionRequest) (*persistence.GetWorkflowExecutionResponse, error) {
	var response *persistence.GetWorkflowExecutionResponse
	op := func() error {
		var err error
		response, err = c.executionManager.GetWorkflowExecution(request)

		return err
	}

	err := backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithRetry(
	request *persistence.UpdateWorkflowExecutionRequest) (*persistence.UpdateWorkflowExecutionResponse, error) {
	resp := &persistence.UpdateWorkflowExecutionResponse{}
	op := func() error {
		var err error
		resp, err = c.shard.UpdateWorkflowExecution(request)
		return err
	}

	err := backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	return resp, err
}

func (c *workflowExecutionContext) clear() {
	c.metricsClient.IncCounter(metrics.WorkflowContextScope, metrics.WorkflowContextCleared)
	c.msBuilder = nil
}

// scheduleNewDecision is helper method which has the logic for scheduling new decision for a workflow execution.
// This function takes in a slice of transferTasks and timerTasks already scheduled for the current transaction
// and may append more tasks to it.  It also returns back the slice with new tasks appended to it.  It is expected
// caller to assign returned slice to original passed in slices.  For this reason we return the original slices
// even if the method fails due to an error on loading workflow execution.
func (c *workflowExecutionContext) scheduleNewDecision(transferTasks []persistence.Task,
	timerTasks []persistence.Task) ([]persistence.Task, []persistence.Task, error) {
	msBuilder, err := c.loadWorkflowExecution()
	if err != nil {
		return transferTasks, timerTasks, err
	}

	executionInfo := msBuilder.GetExecutionInfo()
	if !msBuilder.HasPendingDecisionTask() {
		di := msBuilder.AddDecisionTaskScheduledEvent()
		if di == nil {
			return nil, nil, &workflow.InternalServiceError{Message: "Failed to add decision scheduled event."}
		}
		transferTasks = append(transferTasks, &persistence.DecisionTask{
			DomainID:   executionInfo.DomainID,
			TaskList:   di.TaskList,
			ScheduleID: di.ScheduleID,
		})
		if msBuilder.IsStickyTaskListEnabled() {
			tBuilder := newTimerBuilder(c.shard.GetConfig(), c.logger, common.NewRealTimeSource())
			stickyTaskTimeoutTimer := tBuilder.AddScheduleToStartDecisionTimoutTask(di.ScheduleID, di.Attempt,
				executionInfo.StickyScheduleToStartTimeout)
			timerTasks = append(timerTasks, stickyTaskTimeoutTimer)
		}
	}

	return transferTasks, timerTasks, nil
}

func (c *workflowExecutionContext) emitWorkflowExecutionStats(stats *persistence.MutableStateStats, executionInfoHistorySize int64) {
	if stats == nil {
		return
	}
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.HistorySize,
		time.Duration(executionInfoHistorySize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.MutableStateSize,
		time.Duration(stats.MutableStateSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.ExecutionInfoSize,
		time.Duration(stats.MutableStateSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.ActivityInfoSize,
		time.Duration(stats.ActivityInfoSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.TimerInfoSize,
		time.Duration(stats.TimerInfoSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.ChildInfoSize,
		time.Duration(stats.ChildInfoSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.SignalInfoSize,
		time.Duration(stats.SignalInfoSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.BufferedEventsSize,
		time.Duration(stats.BufferedEventsSize))
	c.metricsClient.RecordTimer(metrics.ExecutionSizeStatsScope, metrics.BufferedReplicationTasksSize,
		time.Duration(stats.BufferedReplicationTasksSize))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.ActivityInfoCount,
		time.Duration(stats.ActivityInfoCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.TimerInfoCount,
		time.Duration(stats.TimerInfoCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.ChildInfoCount,
		time.Duration(stats.ChildInfoCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.SignalInfoCount,
		time.Duration(stats.SignalInfoCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.RequestCancelInfoCount,
		time.Duration(stats.RequestCancelInfoCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.BufferedEventsCount,
		time.Duration(stats.BufferedEventsCount))
	c.metricsClient.RecordTimer(metrics.ExecutionCountStatsScope, metrics.BufferedReplicationTasksCount,
		time.Duration(stats.BufferedReplicationTasksCount))
}

func (c *workflowExecutionContext) emitSessionUpdateStats(stats *persistence.MutableStateUpdateSessionStats) {
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.MutableStateSize,
		time.Duration(stats.MutableStateSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.ExecutionInfoSize,
		time.Duration(stats.ExecutionInfoSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.ActivityInfoSize,
		time.Duration(stats.ActivityInfoSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.TimerInfoSize,
		time.Duration(stats.TimerInfoSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.ChildInfoSize,
		time.Duration(stats.ChildInfoSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.SignalInfoSize,
		time.Duration(stats.SignalInfoSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.BufferedEventsSize,
		time.Duration(stats.BufferedEventsSize))
	c.metricsClient.RecordTimer(metrics.SessionSizeStatsScope, metrics.BufferedReplicationTasksSize,
		time.Duration(stats.BufferedReplicationTasksSize))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.ActivityInfoCount,
		time.Duration(stats.ActivityInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.TimerInfoCount,
		time.Duration(stats.TimerInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.ChildInfoCount,
		time.Duration(stats.ChildInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.SignalInfoCount,
		time.Duration(stats.SignalInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.RequestCancelInfoCount,
		time.Duration(stats.RequestCancelInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.DeleteActivityInfoCount,
		time.Duration(stats.DeleteActivityInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.DeleteTimerInfoCount,
		time.Duration(stats.DeleteTimerInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.DeleteChildInfoCount,
		time.Duration(stats.DeleteChildInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.DeleteSignalInfoCount,
		time.Duration(stats.DeleteSignalInfoCount))
	c.metricsClient.RecordTimer(metrics.SessionCountStatsScope, metrics.DeleteRequestCancelInfoCount,
		time.Duration(stats.DeleteRequestCancelInfoCount))
}
