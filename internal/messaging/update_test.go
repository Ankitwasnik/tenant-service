package messaging_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
)

func TestUpdateID(t *testing.T) {
	task := uuid.MustParse("0192a000-0000-7000-8000-000000000001")
	other := uuid.MustParse("0192a000-0000-7000-8000-000000000002")

	t.Run("deterministic", func(t *testing.T) {
		// A second worker run of the same task must produce the same id.
		first := messaging.UpdateID(task, domain.TaskDone)
		second := messaging.UpdateID(uuid.MustParse(task.String()), domain.TaskStatus("done"))
		if first != second {
			t.Fatal("same task and status gave different ids")
		}
	})
	t.Run("differs per status", func(t *testing.T) {
		seen := map[uuid.UUID]domain.TaskStatus{}
		for _, s := range []domain.TaskStatus{domain.TaskInProgress, domain.TaskDone, domain.TaskFailed} {
			id := messaging.UpdateID(task, s)
			if prev, dup := seen[id]; dup {
				t.Fatalf("%s and %s share an id", prev, s)
			}
			seen[id] = s
		}
	})
	t.Run("differs per task", func(t *testing.T) {
		if messaging.UpdateID(task, domain.TaskDone) == messaging.UpdateID(other, domain.TaskDone) {
			t.Fatal("two tasks share an update id")
		}
	})
	t.Run("is a version 5 UUID", func(t *testing.T) {
		if v := messaging.UpdateID(task, domain.TaskDone).Version(); v != 5 {
			t.Fatalf("version = %d", v)
		}
	})
	t.Run("stable across releases", func(t *testing.T) {
		// Pinned: changing the namespace or the name format would give a re-run
		// of an old task new ids, and the inbox would stop deduplicating them.
		want := uuid.NewSHA1(uuid.MustParse("3fdf24c7-f60c-465a-855c-5c8d6bb88c9c"), []byte(task.String()+":done"))
		if got := messaging.UpdateID(task, domain.TaskDone); got != want {
			t.Fatalf("UpdateID = %s, want %s", got, want)
		}
	})
}

func TestNewTaskUpdate(t *testing.T) {
	task := uuid.New()

	failed := messaging.NewTaskUpdate(task, domain.TaskFailed, "disk full")
	if failed.Error != "disk full" || failed.UpdateID != messaging.UpdateID(task, domain.TaskFailed) || failed.TaskID != task {
		t.Errorf("failed update = %+v", failed)
	}
	// An error message is only kept on a failed update, so the result always validates.
	done := messaging.NewTaskUpdate(task, domain.TaskDone, "ignored")
	if done.Error != "" {
		t.Errorf("done update kept error %q", done.Error)
	}
	for _, u := range []messaging.TaskUpdate{failed, done} {
		if err := u.Validate(); err != nil {
			t.Errorf("NewTaskUpdate built an invalid update: %v", err)
		}
	}
}

func TestTaskUpdateValidate(t *testing.T) {
	valid := func() messaging.TaskUpdate {
		return messaging.TaskUpdate{UpdateID: uuid.New(), TaskID: uuid.New(), Status: domain.TaskDone}
	}

	tests := []struct {
		name  string
		edit  func(*messaging.TaskUpdate)
		valid bool
	}{
		{"done", func(*messaging.TaskUpdate) {}, true},
		{"in_progress", func(u *messaging.TaskUpdate) { u.Status = domain.TaskInProgress }, true},
		{"failed with error", func(u *messaging.TaskUpdate) { u.Status, u.Error = domain.TaskFailed, "boom" }, true},
		{"failed without error", func(u *messaging.TaskUpdate) { u.Status = domain.TaskFailed }, true},
		{"missing update_id", func(u *messaging.TaskUpdate) { u.UpdateID = uuid.Nil }, false},
		{"missing task_id", func(u *messaging.TaskUpdate) { u.TaskID = uuid.Nil }, false},
		{"accepted", func(u *messaging.TaskUpdate) { u.Status = domain.TaskAccepted }, false},
		{"unknown status", func(u *messaging.TaskUpdate) { u.Status = "finished" }, false},
		{"empty status", func(u *messaging.TaskUpdate) { u.Status = "" }, false},
		{"error on done", func(u *messaging.TaskUpdate) { u.Error = "boom" }, false},
		{"error on in_progress", func(u *messaging.TaskUpdate) { u.Status, u.Error = domain.TaskInProgress, "boom" }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := valid()
			tt.edit(&u)
			err := u.Validate()
			if tt.valid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.valid && !errors.Is(err, messaging.ErrInvalidMessage) {
				t.Fatalf("err = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestDecodeTaskUpdate(t *testing.T) {
	good := `{"update_id":"5b1f0000-0000-5000-8000-000000000001","task_id":"0192a000-0000-7000-8000-000000000001","status":"done"}`

	t.Run("valid", func(t *testing.T) {
		u, err := messaging.DecodeTaskUpdate([]byte(good))
		if err != nil || u.Status != domain.TaskDone || u.TaskID.String() != "0192a000-0000-7000-8000-000000000001" {
			t.Fatalf("u=%+v err=%v", u, err)
		}
	})
	t.Run("unknown fields ignored", func(t *testing.T) {
		body := `{"update_id":"5b1f0000-0000-5000-8000-000000000001","task_id":"0192a000-0000-7000-8000-000000000001","status":"done","worker":"w-1"}`
		if _, err := messaging.DecodeTaskUpdate([]byte(body)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	for name, body := range map[string]string{
		"not JSON":          `not json`,
		"empty":             ``,
		"JSON array":        `[]`,
		"malformed task_id": `{"update_id":"5b1f0000-0000-5000-8000-000000000001","task_id":"nope","status":"done"}`,
		"numeric update_id": `{"update_id":42,"task_id":"0192a000-0000-7000-8000-000000000001","status":"done"}`,
		"missing fields":    `{}`,
		"accepted":          `{"update_id":"5b1f0000-0000-5000-8000-000000000001","task_id":"0192a000-0000-7000-8000-000000000001","status":"accepted"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := messaging.DecodeTaskUpdate([]byte(body)); !errors.Is(err, messaging.ErrInvalidMessage) {
				t.Fatalf("err = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestRoutingKeys(t *testing.T) {
	if got := messaging.UpdateRoutingKey(domain.TaskInProgress); got != "task.update.in_progress" {
		t.Errorf("UpdateRoutingKey = %q", got)
	}
	if got := messaging.TaskRoutingKey(domain.TaskDeploy); got != "task.deploy" {
		t.Errorf("TaskRoutingKey = %q", got)
	}
}

func TestTaskEventJSON(t *testing.T) {
	task := domain.Task{
		ID: uuid.New(), TenantID: uuid.New(), Type: domain.TaskUpdate, Status: domain.TaskAccepted,
		CreatedAt: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(messaging.NewTaskEvent(task))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"id": task.ID.String(), "type": "update", "tenant_id": task.TenantID.String(),
		"status": "accepted", "created_at": "2026-09-26T10:00:00.000Z",
	}
	if len(got) != len(want) {
		t.Fatalf("event = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
