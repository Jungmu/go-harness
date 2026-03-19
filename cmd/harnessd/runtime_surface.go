package main

import (
	"slices"
	"sort"
	"strings"
	"time"

	"go-harness/internal/domain"
)

type runtimeSurface struct {
	codingSnapshot      func() domain.StateSnapshot
	reviewSnapshot      func() domain.StateSnapshot
	codingIssueSnapshot func(string) (domain.IssueRuntimeSnapshot, bool)
	reviewIssueSnapshot func(string) (domain.IssueRuntimeSnapshot, bool)
	codingRefresh       func()
	reviewRefresh       func()
}

func (r *runtimeSurface) Snapshot() domain.StateSnapshot {
	coding := r.codingSnapshot()
	if r.reviewSnapshot == nil {
		coding.Dispatch.Workers = []domain.WorkerDispatchStatus{{
			Worker:  "coding",
			Blocked: coding.Dispatch.Blocked,
			Error:   coding.Dispatch.Error,
		}}
		return coding
	}
	review := r.reviewSnapshot()
	return mergeStateSnapshots(coding, review)
}

func (r *runtimeSurface) IssueSnapshot(identifier string) (domain.IssueRuntimeSnapshot, bool) {
	coding, codingOK := r.codingIssueSnapshot(identifier)
	if r.reviewIssueSnapshot == nil {
		return coding, codingOK
	}
	review, reviewOK := r.reviewIssueSnapshot(identifier)
	return mergeIssueSnapshots(coding, codingOK, review, reviewOK)
}

func (r *runtimeSurface) TriggerRefresh() {
	if r.codingRefresh != nil {
		r.codingRefresh()
	}
	if r.reviewRefresh != nil {
		r.reviewRefresh()
	}
}

func mergeStateSnapshots(coding, review domain.StateSnapshot) domain.StateSnapshot {
	merged := coding
	merged.GeneratedAt = maxTime(coding.GeneratedAt, review.GeneratedAt)
	merged.Workflow.ReviewPath = review.Workflow.Path
	merged.Counts.Running = coding.Counts.Running + review.Counts.Running
	merged.Counts.Retrying = coding.Counts.Retrying + review.Counts.Retrying
	merged.Dispatch = mergeDispatchStatus(coding.Dispatch, review.Dispatch)
	merged.Running = mergeRunningSnapshots(coding.Running, review.Running)
	merged.Retrying = mergeRetryEntries(coding.Retrying, review.Retrying)
	merged.RecentActivity = mergeTimelineEvents(coding.RecentActivity, review.RecentActivity)
	merged.CodexTotals = domain.RuntimeTotals{
		InputTokens:    coding.CodexTotals.InputTokens + review.CodexTotals.InputTokens,
		OutputTokens:   coding.CodexTotals.OutputTokens + review.CodexTotals.OutputTokens,
		TotalTokens:    coding.CodexTotals.TotalTokens + review.CodexTotals.TotalTokens,
		SecondsRunning: coding.CodexTotals.SecondsRunning + review.CodexTotals.SecondsRunning,
	}
	merged.RateLimits = mergeRateLimits(coding.RateLimits, review.RateLimits)
	merged.Completed = mergeCompleted(coding.Completed, review.Completed)
	return merged
}

func mergeIssueSnapshots(coding domain.IssueRuntimeSnapshot, codingOK bool, review domain.IssueRuntimeSnapshot, reviewOK bool) (domain.IssueRuntimeSnapshot, bool) {
	if !codingOK && !reviewOK {
		return domain.IssueRuntimeSnapshot{}, false
	}
	if !codingOK {
		return review, true
	}
	if !reviewOK {
		return coding, true
	}

	merged := domain.IssueRuntimeSnapshot{
		GeneratedAt: maxTime(coding.GeneratedAt, review.GeneratedAt),
		Identifier:  coding.Identifier,
		History:     mergeTimelineEvents(coding.History, review.History),
		Completed:   coding.Completed || review.Completed,
	}

	switch {
	case coding.Running != nil:
		merged.Status = "running"
		merged.Running = cloneRunningSnapshot(coding.Running)
	case review.Running != nil:
		merged.Status = "running"
		merged.Running = cloneRunningSnapshot(review.Running)
	case coding.Retry != nil:
		merged.Status = "retrying"
		retry := *coding.Retry
		merged.Retry = &retry
	case review.Retry != nil:
		merged.Status = "retrying"
		retry := *review.Retry
		merged.Retry = &retry
	case merged.Completed:
		merged.Status = "completed"
	default:
		merged.Status = "observed"
	}
	return merged, true
}

func mergeDispatchStatus(coding, review domain.DispatchStatus) domain.DispatchStatus {
	workers := []domain.WorkerDispatchStatus{
		{Worker: "coding", Blocked: coding.Blocked, Error: coding.Error},
		{Worker: "review", Blocked: review.Blocked, Error: review.Error},
	}
	errorsOut := make([]string, 0, len(workers))
	for _, worker := range workers {
		if worker.Blocked && strings.TrimSpace(worker.Error) != "" {
			errorsOut = append(errorsOut, worker.Worker+": "+worker.Error)
		}
	}
	return domain.DispatchStatus{
		Blocked: coding.Blocked || review.Blocked,
		Error:   strings.Join(errorsOut, "; "),
		Workers: workers,
	}
}

func mergeRunningSnapshots(coding, review []domain.RunningSnapshot) []domain.RunningSnapshot {
	if len(coding)+len(review) == 0 {
		return nil
	}
	merged := make([]domain.RunningSnapshot, 0, len(coding)+len(review))
	for _, item := range coding {
		merged = append(merged, cloneRunningValue(item))
	}
	for _, item := range review {
		merged = append(merged, cloneRunningValue(item))
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Issue.Identifier != merged[j].Issue.Identifier {
			return merged[i].Issue.Identifier < merged[j].Issue.Identifier
		}
		return runningWorkerName(merged[i]) < runningWorkerName(merged[j])
	})
	return merged
}

func mergeRetryEntries(coding, review []domain.RetryEntry) []domain.RetryEntry {
	if len(coding)+len(review) == 0 {
		return nil
	}
	merged := make([]domain.RetryEntry, 0, len(coding)+len(review))
	merged = append(merged, coding...)
	merged = append(merged, review...)
	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].DueAt.Equal(merged[j].DueAt) {
			return merged[i].DueAt.Before(merged[j].DueAt)
		}
		return merged[i].Identifier < merged[j].Identifier
	})
	return merged
}

func mergeTimelineEvents(coding, review []domain.TimelineEvent) []domain.TimelineEvent {
	if len(coding)+len(review) == 0 {
		return nil
	}
	merged := make([]domain.TimelineEvent, 0, len(coding)+len(review))
	merged = append(merged, coding...)
	merged = append(merged, review...)
	sort.SliceStable(merged, func(i, j int) bool {
		if !merged[i].At.Equal(merged[j].At) {
			return merged[i].At.After(merged[j].At)
		}
		if merged[i].Identifier != merged[j].Identifier {
			return merged[i].Identifier < merged[j].Identifier
		}
		return merged[i].Event < merged[j].Event
	})
	return merged
}

func mergeRateLimits(coding, review []domain.RateLimitSnapshot) []domain.RateLimitSnapshot {
	if len(coding)+len(review) == 0 {
		return nil
	}
	byProvider := make(map[string]domain.RateLimitSnapshot, len(coding)+len(review))
	for _, snapshot := range append(append([]domain.RateLimitSnapshot{}, coding...), review...) {
		current, ok := byProvider[snapshot.Provider]
		if !ok || snapshot.UpdatedAt.After(current.UpdatedAt) {
			byProvider[snapshot.Provider] = snapshot
		}
	}
	out := make([]domain.RateLimitSnapshot, 0, len(byProvider))
	for _, snapshot := range byProvider {
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Provider < out[j].Provider
	})
	return out
}

func mergeCompleted(coding, review []string) []string {
	if len(coding)+len(review) == 0 {
		return nil
	}
	merged := append(append([]string{}, coding...), review...)
	sort.Strings(merged)
	return slices.Compact(merged)
}

func cloneRunningSnapshot(snapshot *domain.RunningSnapshot) *domain.RunningSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := cloneRunningValue(*snapshot)
	return &cloned
}

func cloneRunningValue(snapshot domain.RunningSnapshot) domain.RunningSnapshot {
	cloned := snapshot
	if snapshot.LiveSession != nil {
		liveSession := *snapshot.LiveSession
		cloned.LiveSession = &liveSession
	}
	if len(snapshot.RecentEvents) > 0 {
		cloned.RecentEvents = append([]domain.RecentEvent{}, snapshot.RecentEvents...)
	}
	return cloned
}

func runningWorkerName(snapshot domain.RunningSnapshot) string {
	if snapshot.LiveSession == nil {
		return ""
	}
	return snapshot.LiveSession.Worker
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
