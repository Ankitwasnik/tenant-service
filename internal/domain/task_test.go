package domain_test

import (
	"testing"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

var allTaskStatuses = []domain.TaskStatus{
	domain.TaskAccepted,
	domain.TaskInProgress,
	domain.TaskDone,
	domain.TaskFailed,
}

func TestShouldApply(t *testing.T) {
	// Every (current, incoming) pair. Only forward moves apply.
	want := map[domain.TaskStatus]map[domain.TaskStatus]bool{
		domain.TaskAccepted: {
			domain.TaskAccepted:   false,
			domain.TaskInProgress: true,
			domain.TaskDone:       true, // done before in_progress: a forward jump
			domain.TaskFailed:     true,
		},
		domain.TaskInProgress: {
			domain.TaskAccepted:   false,
			domain.TaskInProgress: false, // duplicate
			domain.TaskDone:       true,
			domain.TaskFailed:     true,
		},
		domain.TaskDone: { // terminal: nothing applies
			domain.TaskAccepted:   false,
			domain.TaskInProgress: false, // in_progress arriving after done
			domain.TaskDone:       false,
			domain.TaskFailed:     false,
		},
		domain.TaskFailed: { // terminal: nothing applies
			domain.TaskAccepted:   false,
			domain.TaskInProgress: false,
			domain.TaskDone:       false,
			domain.TaskFailed:     false,
		},
	}

	for _, current := range allTaskStatuses {
		for _, incoming := range allTaskStatuses {
			t.Run(string(current)+"->"+string(incoming), func(t *testing.T) {
				if got := domain.ShouldApply(current, incoming); got != want[current][incoming] {
					t.Errorf("ShouldApply = %v, want %v", got, want[current][incoming])
				}
			})
		}
	}
}

func TestShouldApplyUnknownStatus(t *testing.T) {
	if domain.ShouldApply(domain.TaskAccepted, "bogus") {
		t.Error("unknown incoming status applied")
	}
	if domain.ShouldApply("bogus", domain.TaskDone) {
		t.Error("update applied to a task with an unknown status")
	}
}

func TestTerminalOutcome(t *testing.T) {
	tests := []struct {
		taskType     domain.TaskType
		status       domain.TaskStatus
		wantOK       bool
		wantNext     domain.TenantStatus
		wantExpected domain.TenantStatus
	}{
		{domain.TaskDeploy, domain.TaskAccepted, false, "", ""},
		{domain.TaskDeploy, domain.TaskInProgress, false, "", ""},
		{domain.TaskDeploy, domain.TaskDone, true, domain.TenantActive, domain.TenantProvisioning},
		{domain.TaskDeploy, domain.TaskFailed, true, domain.TenantFailed, domain.TenantProvisioning},

		{domain.TaskUpdate, domain.TaskAccepted, false, "", ""},
		{domain.TaskUpdate, domain.TaskInProgress, false, "", ""},
		{domain.TaskUpdate, domain.TaskDone, true, domain.TenantActive, domain.TenantUpdating},
		{domain.TaskUpdate, domain.TaskFailed, true, domain.TenantFailed, domain.TenantUpdating},

		{domain.TaskDestroy, domain.TaskAccepted, false, "", ""},
		{domain.TaskDestroy, domain.TaskInProgress, false, "", ""},
		{domain.TaskDestroy, domain.TaskDone, true, domain.TenantDestroyed, domain.TenantDestroying},
		{domain.TaskDestroy, domain.TaskFailed, true, domain.TenantFailed, domain.TenantDestroying},

		{"bogus", domain.TaskDone, false, "", ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.taskType)+"/"+string(tt.status), func(t *testing.T) {
			next, expected, ok := domain.TerminalOutcome(tt.taskType, tt.status)
			if ok != tt.wantOK || next != tt.wantNext || expected != tt.wantExpected {
				t.Errorf("TerminalOutcome = (%q, %q, %v), want (%q, %q, %v)",
					next, expected, ok, tt.wantNext, tt.wantExpected, tt.wantOK)
			}
		})
	}
}

func TestStatusValidity(t *testing.T) {
	for _, s := range allTenantStatuses {
		if !s.Valid() {
			t.Errorf("tenant status %q not valid", s)
		}
	}
	for _, s := range allTaskStatuses {
		if !s.Valid() {
			t.Errorf("task status %q not valid", s)
		}
	}
	for _, typ := range []domain.TaskType{domain.TaskDeploy, domain.TaskUpdate, domain.TaskDestroy} {
		if !typ.Valid() {
			t.Errorf("task type %q not valid", typ)
		}
	}

	if domain.TenantStatus("deleted").Valid() || domain.TaskStatus("pending").Valid() || domain.TaskType("create").Valid() {
		t.Error("an undefined value reported valid")
	}
	if domain.TenantStatus("").Valid() || domain.TaskStatus("").Valid() || domain.TaskType("").Valid() {
		t.Error("the empty value reported valid")
	}
}

func TestTaskStatusTerminal(t *testing.T) {
	want := map[domain.TaskStatus]bool{
		domain.TaskAccepted:   false,
		domain.TaskInProgress: false,
		domain.TaskDone:       true,
		domain.TaskFailed:     true,
	}
	for s, terminal := range want {
		if s.Terminal() != terminal {
			t.Errorf("%q.Terminal() = %v, want %v", s, s.Terminal(), terminal)
		}
	}
}
