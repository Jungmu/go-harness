package main

import (
	"testing"
	"time"

	"go-harness/internal/domain"
)

func TestRuntimeSurfaceMergesCodingAndReviewSnapshots(t *testing.T) {
	t.Parallel()

	surface := &runtimeSurface{
		codingSnapshot: func() domain.StateSnapshot {
			return domain.StateSnapshot{
				GeneratedAt: time.Date(2026, 3, 19, 8, 0, 0, 0, time.UTC),
				Workflow:    domain.WorkflowStatus{Path: "/repo/WORKFLOW.md"},
				Counts:      domain.SnapshotCounts{Running: 1, Retrying: 1},
				Dispatch:    domain.DispatchStatus{Blocked: false},
				Running: []domain.RunningSnapshot{{
					Issue: domain.Issue{Identifier: "ABC-1"},
					LiveSession: &domain.LiveSession{
						SessionID: "coding-session",
						Worker:    "coding",
					},
				}},
				Retrying: []domain.RetryEntry{{Identifier: "ABC-2", DueAt: time.Date(2026, 3, 19, 8, 5, 0, 0, time.UTC)}},
				RecentActivity: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 19, 8, 0, 10, 0, time.UTC), Identifier: "ABC-1", Event: "issue_claimed"},
				},
				CodexTotals: domain.RuntimeTotals{TotalTokens: 10},
				Completed:   []string{"ABC-3"},
			}
		},
		reviewSnapshot: func() domain.StateSnapshot {
			return domain.StateSnapshot{
				GeneratedAt: time.Date(2026, 3, 19, 8, 1, 0, 0, time.UTC),
				Workflow:    domain.WorkflowStatus{Path: "/repo/REVIEW-WORKFLOW.md"},
				Counts:      domain.SnapshotCounts{Running: 1, Retrying: 0},
				Dispatch:    domain.DispatchStatus{Blocked: true, Error: "review invalid"},
				Running: []domain.RunningSnapshot{{
					Issue: domain.Issue{Identifier: "ABC-4"},
					LiveSession: &domain.LiveSession{
						SessionID: "review-session",
						Worker:    "review",
					},
				}},
				RecentActivity: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 19, 8, 0, 20, 0, time.UTC), Identifier: "ABC-4", Event: "turn_completed"},
				},
				CodexTotals: domain.RuntimeTotals{TotalTokens: 4},
				Completed:   []string{"ABC-5"},
			}
		},
		codingIssueSnapshot: func(string) (domain.IssueRuntimeSnapshot, bool) { return domain.IssueRuntimeSnapshot{}, false },
		reviewIssueSnapshot: func(string) (domain.IssueRuntimeSnapshot, bool) { return domain.IssueRuntimeSnapshot{}, false },
	}

	snapshot := surface.Snapshot()
	if snapshot.Workflow.Path != "/repo/WORKFLOW.md" || snapshot.Workflow.ReviewPath != "/repo/REVIEW-WORKFLOW.md" {
		t.Fatalf("workflow = %#v", snapshot.Workflow)
	}
	if snapshot.Counts.Running != 2 || snapshot.Counts.Retrying != 1 {
		t.Fatalf("counts = %#v", snapshot.Counts)
	}
	if !snapshot.Dispatch.Blocked || len(snapshot.Dispatch.Workers) != 2 {
		t.Fatalf("dispatch = %#v", snapshot.Dispatch)
	}
	if snapshot.CodexTotals.TotalTokens != 14 {
		t.Fatalf("total tokens = %d, want 14", snapshot.CodexTotals.TotalTokens)
	}
	if len(snapshot.Running) != 2 || snapshot.Running[0].LiveSession.Worker != "coding" || snapshot.Running[1].LiveSession.Worker != "review" {
		t.Fatalf("running = %#v", snapshot.Running)
	}
	if len(snapshot.Completed) != 2 {
		t.Fatalf("completed = %#v", snapshot.Completed)
	}
	if len(snapshot.RecentActivity) != 2 || snapshot.RecentActivity[0].Identifier != "ABC-4" {
		t.Fatalf("recent activity = %#v", snapshot.RecentActivity)
	}
}

func TestRuntimeSurfaceMergesIssueSnapshotsAndRefreshesBoth(t *testing.T) {
	t.Parallel()

	var codingRefresh, reviewRefresh int
	surface := &runtimeSurface{
		codingSnapshot: func() domain.StateSnapshot { return domain.StateSnapshot{} },
		reviewSnapshot: func() domain.StateSnapshot { return domain.StateSnapshot{} },
		codingIssueSnapshot: func(identifier string) (domain.IssueRuntimeSnapshot, bool) {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt: time.Date(2026, 3, 19, 8, 0, 0, 0, time.UTC),
				Identifier:  identifier,
				Status:      "completed",
				Completed:   true,
				History: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 19, 8, 0, 5, 0, time.UTC), Identifier: identifier, Event: "issue_completed"},
				},
			}, true
		},
		reviewIssueSnapshot: func(identifier string) (domain.IssueRuntimeSnapshot, bool) {
			return domain.IssueRuntimeSnapshot{
				GeneratedAt: time.Date(2026, 3, 19, 8, 1, 0, 0, time.UTC),
				Identifier:  identifier,
				Status:      "observed",
				History: []domain.TimelineEvent{
					{At: time.Date(2026, 3, 19, 8, 0, 10, 0, time.UTC), Identifier: identifier, Event: "issue_released"},
				},
			}, true
		},
		codingRefresh: func() { codingRefresh++ },
		reviewRefresh: func() { reviewRefresh++ },
	}

	issue, ok := surface.IssueSnapshot("ABC-1")
	if !ok {
		t.Fatal("IssueSnapshot() = not found, want merged issue")
	}
	if issue.Status != "completed" || !issue.Completed {
		t.Fatalf("issue = %#v", issue)
	}
	if len(issue.History) != 2 || issue.History[0].Event != "issue_released" {
		t.Fatalf("history = %#v", issue.History)
	}

	surface.TriggerRefresh()
	if codingRefresh != 1 || reviewRefresh != 1 {
		t.Fatalf("refresh counts = coding:%d review:%d", codingRefresh, reviewRefresh)
	}
}
