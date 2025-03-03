// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
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

package reclaimresources

import (
	stderrors "errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/service/worker/deletenamespace/deleteexecutions"
	"go.temporal.io/server/service/worker/deletenamespace/errors"
)

const (
	WorkflowName = "temporal-sys-reclaim-namespace-resources-workflow"

	namespaceCacheRefreshDelay = 11 * time.Second
)

type (
	ReclaimResourcesParams struct {
		deleteexecutions.DeleteExecutionsParams

		// NamespaceDeleteDelay indicates the duration for how long ReclaimResourcesWorkflow
		// will sleep between workflow execution and namespace deletion.
		// Default is 0, means, workflow won't sleep.
		NamespaceDeleteDelay time.Duration
	}

	ReclaimResourcesResult struct {
		DeleteSuccessCount int
		DeleteErrorCount   int
		NamespaceDeleted   bool
	}
)

var (
	retryPolicy = &temporal.RetryPolicy{
		InitialInterval: 1 * time.Second,
		MaximumInterval: 10 * time.Second,
	}

	localActivityOptions = workflow.LocalActivityOptions{
		RetryPolicy:            retryPolicy,
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 5 * time.Minute,
	}

	deleteExecutionsWorkflowOptions = workflow.ChildWorkflowOptions{
		RetryPolicy: retryPolicy,
	}

	ensureNoExecutionsActivityRetryPolicy = &temporal.RetryPolicy{
		InitialInterval:    1 * time.Second,
		MaximumInterval:    2 * time.Minute,
		BackoffCoefficient: 2,
	}

	ensureNoExecutionsAdvVisibilityActivityOptions = workflow.ActivityOptions{
		RetryPolicy:            ensureNoExecutionsActivityRetryPolicy,
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 10 * time.Hour, // Sanity check, advanced visibility can control the progress of activity.
	}
)

func validateParams(params *ReclaimResourcesParams) error {
	if params.NamespaceID.IsEmpty() {
		return temporal.NewNonRetryableApplicationError("namespace ID is required", "", nil)
	}

	if params.Namespace.IsEmpty() {
		return temporal.NewNonRetryableApplicationError("namespace is required", "", nil)
	}

	params.Config.ApplyDefaults()

	return nil
}

func ReclaimResourcesWorkflow(ctx workflow.Context, params ReclaimResourcesParams) (ReclaimResourcesResult, error) {
	logger := log.With(
		workflow.GetLogger(ctx),
		tag.WorkflowType(WorkflowName),
		tag.WorkflowNamespace(params.Namespace.String()),
		tag.WorkflowNamespaceID(params.NamespaceID.String()))
	logger.Info("Workflow started.")

	var result ReclaimResourcesResult
	if err := validateParams(&params); err != nil {
		return result, err
	}

	mh := workflow.GetMetricsHandler(ctx).WithTags(map[string]string{"namespace": params.Namespace.String()})
	defer func() {
		if result.NamespaceDeleted {
			mh.Counter(metrics.ReclaimResourcesNamespaceDeleteSuccessCount.Name()).Inc(1)
		} else {
			mh.Counter(metrics.ReclaimResourcesNamespaceDeleteFailureCount.Name()).Inc(1)
		}
		if result.DeleteSuccessCount > 0 {
			mh.Counter(metrics.ReclaimResourcesDeleteExecutionsSuccessCount.Name()).Inc(int64(result.DeleteSuccessCount))
		}
		if result.DeleteErrorCount > 0 {
			mh.Counter(metrics.ReclaimResourcesDeleteExecutionsFailureCount.Name()).Inc(int64(result.DeleteErrorCount))
		}
	}()

	ctx = workflow.WithTaskQueue(ctx, primitives.DeleteNamespaceActivityTQ)

	var (
		namespaceDeleteDelay = params.NamespaceDeleteDelay
		cancelDeleteDelay    workflow.CancelFunc
	)
	err := workflow.SetUpdateHandlerWithOptions(ctx, "update_namespace_delete_delay", func(ctx workflow.Context, newNamespaceDeleteDelayStr string) (string, error) {
		// This must succeed because Update validator already validated the input.
		namespaceDeleteDelay, _ = time.ParseDuration(newNamespaceDeleteDelayStr)

		var updateResult string
		if namespaceDeleteDelay == 0 {
			logger.Info("Namespace delete delay is removed. Namespace will be deleted immediately after all workflow executions are deleted.")
			updateResult = "Namespace delete delay is removed."
		} else {
			logger.Info("Namespace delete delay is updated.", "new-delete-delay", namespaceDeleteDelay)
			updateResult = fmt.Sprintf("Namespace delete delay is updated to %s.", namespaceDeleteDelay)
		}

		if cancelDeleteDelay != nil {
			cancelDeleteDelay()
			logger.Info("Existing namespace delete delay timer is cancelled.")
			updateResult = "Existing namespace delete delay timer is cancelled. " + updateResult
		}
		return updateResult, nil
	}, workflow.UpdateHandlerOptions{
		Validator: func(_ workflow.Context, newNamespaceDeleteDelayStr string) error {
			if newNamespaceDeleteDelayStr == "" {
				return temporal.NewNonRetryableApplicationError("delay duration is required", errors.ValidationErrorErrType, nil)
			}
			newDuration, err := time.ParseDuration(newNamespaceDeleteDelayStr)
			if err != nil {
				return temporal.NewNonRetryableApplicationError("unable to parse delay duration", errors.ValidationErrorErrType, err)
			}
			if newDuration < 0 {
				return temporal.NewNonRetryableApplicationError("delay duration must be positive", errors.ValidationErrorErrType, nil)
			}
			if newDuration > 30*24*time.Hour {
				return temporal.NewNonRetryableApplicationError("delay duration must be less than 30 days", errors.ValidationErrorErrType, nil)
			}
			return nil
		},
	})
	if err != nil {
		return result, fmt.Errorf("%w: %v", errors.ErrUnableToSetUpdateHandler, err)
	}

	var la *LocalActivities

	// Step 0. This workflow is started right after the namespace is marked as DELETED and renamed.
	// Wait for namespace cache refresh to make sure no new executions are created.
	err = workflow.Sleep(ctx, namespaceCacheRefreshDelay)
	if err != nil {
		return result, err
	}

	// Step 1. Delete workflow executions.
	result, err = deleteWorkflowExecutions(ctx, logger, params)
	if err != nil {
		return result, err
	}

	// Step 2. Sleep before deleting namespace from a database.
	for namespaceDeleteDelay > 0 {
		var cancelableCtx workflow.Context
		cancelableCtx, cancelDeleteDelay = workflow.WithCancel(ctx)
		logger.Info("Delaying namespace delete. Send 'update_namespace_delete_delay' update to change or clear the delay.",
			"duration", namespaceDeleteDelay.String())

		ndd := namespaceDeleteDelay
		namespaceDeleteDelay = 0
		_ = workflow.Sleep(cancelableCtx, ndd)
	}

	// Step 3. Delete namespace from database.
	ctx5 := workflow.WithLocalActivityOptions(ctx, localActivityOptions)
	err = workflow.ExecuteLocalActivity(ctx5, la.DeleteNamespaceActivity, params.NamespaceID, params.Namespace).Get(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("%w: DeleteNamespaceActivity: %v", errors.ErrUnableToExecuteActivity, err)
	}

	result.NamespaceDeleted = true
	logger.Info("Workflow finished successfully.")
	return result, nil
}

func deleteWorkflowExecutions(ctx workflow.Context, logger log.Logger, params ReclaimResourcesParams) (ReclaimResourcesResult, error) {
	var a *Activities
	var la *LocalActivities

	var result ReclaimResourcesResult

	ctx1 := workflow.WithLocalActivityOptions(ctx, localActivityOptions)

	// TODO: remove this code branch after v1.26 release
	v := workflow.GetVersion(ctx, "remove-std-vis", workflow.DefaultVersion, 0)
	if v == workflow.DefaultVersion {
		// Standard visibility was removed from server codebase since v1.24 release. We don't need to call this local
		// activity to know if it is advanced visibility, we know it is true. However, we need to keep it here so that
		// it can replay workflow history generated before this change.
		var isAdvancedVisibility bool
		err := workflow.ExecuteLocalActivity(ctx1, la.IsAdvancedVisibilityActivity, params.Namespace).Get(ctx, &isAdvancedVisibility)
		if err != nil {
			return result, fmt.Errorf("%w: IsAdvancedVisibilityActivity: %v", errors.ErrUnableToExecuteActivity, err)
		}
	}

	ctx4 := workflow.WithLocalActivityOptions(ctx, localActivityOptions)
	var executionsCount int64
	err := workflow.ExecuteLocalActivity(ctx4, la.CountExecutionsAdvVisibilityActivity, params.NamespaceID, params.Namespace).Get(ctx, &executionsCount)
	if err != nil {
		return result, fmt.Errorf("%w: CountExecutionsAdvVisibilityActivity: %v", errors.ErrUnableToExecuteActivity, err)
	}
	if executionsCount == 0 {
		return result, nil
	}

	ctx2 := workflow.WithChildOptions(ctx, deleteExecutionsWorkflowOptions)
	ctx2 = workflow.WithWorkflowID(ctx2, fmt.Sprintf("%s/%s", deleteexecutions.WorkflowName, params.Namespace))
	var der deleteexecutions.DeleteExecutionsResult
	err = workflow.ExecuteChildWorkflow(ctx2, deleteexecutions.DeleteExecutionsWorkflow, params.DeleteExecutionsParams).Get(ctx, &der)
	if err != nil {
		logger.Error("Unable to execute child workflow.", tag.Error(err))
		return result, fmt.Errorf("%w: %s: %v", errors.ErrUnableToExecuteChildWorkflow, deleteexecutions.WorkflowName, err)
	}
	result.DeleteSuccessCount = der.SuccessCount
	result.DeleteErrorCount = der.ErrorCount

	ctx3 := workflow.WithActivityOptions(ctx, ensureNoExecutionsAdvVisibilityActivityOptions)
	err = workflow.ExecuteActivity(ctx3, a.EnsureNoExecutionsAdvVisibilityActivity, params.NamespaceID, params.Namespace, der.ErrorCount).Get(ctx, nil)
	if err != nil {
		var appErr *temporal.ApplicationError
		if stderrors.As(err, &appErr) {
			switch appErr.Type() {
			case errors.ExecutionsStillExistErrType, errors.NoProgressErrType, errors.NotDeletedExecutionsStillExistErrType:
				var notDeletedCount int
				var counterTag tag.ZapTag
				if appErr.HasDetails() {
					_ = appErr.Details(&notDeletedCount)
					counterTag = tag.Counter(notDeletedCount)
				}
				logger.Info("Unable to delete workflow executions.", counterTag)
				// appErr is not retryable. Convert it to retryable for the server to retry.
				return result, temporal.NewApplicationError(appErr.Message(), appErr.Type(), notDeletedCount)
			}
		}
		return result, fmt.Errorf("%w: EnsureNoExecutionsActivity: %v", errors.ErrUnableToExecuteActivity, err)
	}

	return result, nil
}
