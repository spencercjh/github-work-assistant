package app

import (
	"reflect"
	"testing"
)

func TestReportHistoryOrderSurvivesEquivalentRowUpdates(t *testing.T) {
	s := freshTestStore(t)
	ctx := t.Context()
	key := "o/r#1"
	id, err := s.Observe(ctx, key, canonical(Snapshot{Key: key, Objects: Objects{"issue": {"title": "history"}}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []string{"a", "b", "c"} {
		body := string(canonical(map[string]string{"report_id": report}))
		if err := s.db.Session(ctx).Create(&resultRecord{EventID: id, ReportID: report, Body: body, CreatedAt: 42}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if testDriver() == "postgres" {
		// Earlier tests may leave statistics for much larger tables.
		if _, err := s.db.ExecContext(ctx, "ANALYZE ghraft_events, ghraft_results"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.History(ctx, key)
	if err != nil || len(before) != 3 {
		t.Fatal("missing report history", len(before), err)
	}
	// PostgreSQL can move an updated row without changing any stored value.
	// Equal timestamps must not make unchanged reports appear rewritten.
	if _, err := s.db.ExecContext(ctx, "UPDATE ghraft_results SET body=body WHERE report_id=?", "a"); err != nil {
		t.Fatal(err)
	}
	after, err := s.History(ctx, key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("unchanged history reordered: before=%s after=%s error=%v", canonical(before), canonical(after), err)
	}
}
