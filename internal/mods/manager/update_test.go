package manager

import (
	"context"
	"database/sql"
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
)

func TestWithArchiveRecordsBeforeOutcomeFinish(t *testing.T) {
	var order []string
	archived := func(context.Context, *sql.Tx) error { order = append(order, "archive"); return nil }

	failed := withArchive(jobs.Outcome{Status: jobs.StatusFailed}, archived)
	if failed.OnFinish == nil {
		t.Fatal("a failed update drops the archive it took")
	}
	if err := failed.OnFinish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	then := func(context.Context, *sql.Tx) error { order = append(order, "then"); return nil }
	ok := withArchive(jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: then}, archived)
	if err := ok.OnFinish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[1] != "archive" || order[2] != "then" {
		t.Errorf("order = %v, want the archive before the outcome's own finish", order)
	}
	if out := withArchive(jobs.Outcome{Status: jobs.StatusSucceeded}, nil); out.OnFinish != nil {
		t.Error("no archive still installed a finish hook")
	}
}
