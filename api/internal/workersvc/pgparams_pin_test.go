package workersvc

import "testing"

// ptr returns a pointer to v. Local to this pin file; workersvc's other test
// files declare a differently-typed strptr, so a distinct generic name avoids
// collision.
func ptr[T any](v T) *T { return &v }

// TestTextParamVsPgTextPtrSplit pins the *string→pgtype.Text split that a
// name-based migration would get wrong. textParam (pgparams.go) and pgTextPtr
// (limitwait.go) share the same *string signature but disagree about what &""
// means, and the pgconv migration must preserve that disagreement:
//
//	textParam  → pgconv.TextPtrOrNull  (nil or &"" → NULL)
//	pgTextPtr  → pgconv.TextPtr        (nil → NULL, &"" → valid empty)
func TestTextParamVsPgTextPtrSplit(t *testing.T) {
	// textParam: nil and &"" both collapse to NULL; a non-empty value is valid.
	if got := textParam(nil); got.Valid {
		t.Errorf("textParam(nil): got Valid=%v, want Valid=false (NULL)", got.Valid)
	}
	// LOAD-BEARING: textParam(&"") → NULL. This is the distinction the pgconv
	// migration must preserve — textParam maps ""→NULL (→ pgconv.TextPtrOrNull),
	// unlike pgTextPtr just below which maps ""→valid-empty (→ pgconv.TextPtr).
	if got := textParam(ptr("")); got.Valid {
		t.Errorf("textParam(&\"\"): got Valid=%v, want Valid=false (NULL)", got.Valid)
	}
	if got := textParam(ptr("x")); !got.Valid || got.String != "x" {
		t.Errorf("textParam(&\"x\"): got {Valid=%v String=%q}, want {Valid=true String=\"x\"}", got.Valid, got.String)
	}

	// pgTextPtr: only nil collapses to NULL; &"" is a VALID empty string.
	if got := pgTextPtr(nil); got.Valid {
		t.Errorf("pgTextPtr(nil): got Valid=%v, want Valid=false (NULL)", got.Valid)
	}
	// LOAD-BEARING: pgTextPtr(&"") → valid empty string. The opposite of
	// textParam(&"") above — pgTextPtr maps ""→valid-empty (→ pgconv.TextPtr),
	// and swapping it to textParam's ""→NULL body would corrupt behavior.
	if got := pgTextPtr(ptr("")); !got.Valid || got.String != "" {
		t.Errorf("pgTextPtr(&\"\"): got {Valid=%v String=%q}, want {Valid=true String=\"\"}", got.Valid, got.String)
	}
	if got := pgTextPtr(ptr("x")); !got.Valid || got.String != "x" {
		t.Errorf("pgTextPtr(&\"x\"): got {Valid=%v String=%q}, want {Valid=true String=\"x\"}", got.Valid, got.String)
	}
}

// TestPgInt4ClampVsInt4Ptr pins the int split in upgrade.go. pgInt4's v<=0→NULL
// is a domain clamp (it STAYS put post-migration); int4Ptr only nil→NULL (it
// migrates to pgconv.Int4Ptr32). A wrong swap of one for the other would corrupt
// behavior, since a pointed-to 0 is a VALID 0 for int4Ptr but pgInt4 would NULL it.
func TestPgInt4ClampVsInt4Ptr(t *testing.T) {
	// pgInt4: v<=0 clamps to NULL (domain clamp — stays put).
	if got := pgInt4(0); got.Valid {
		t.Errorf("pgInt4(0): got Valid=%v, want Valid=false (NULL) — zero clamps to NULL", got.Valid)
	}
	if got := pgInt4(-1); got.Valid {
		t.Errorf("pgInt4(-1): got Valid=%v, want Valid=false (NULL) — negative clamps to NULL", got.Valid)
	}
	if got := pgInt4(5); !got.Valid || got.Int32 != 5 {
		t.Errorf("pgInt4(5): got {Valid=%v Int32=%d}, want {Valid=true Int32=5}", got.Valid, got.Int32)
	}

	// int4Ptr: only nil→NULL, no clamp — a pointed-to 0 is a VALID 0.
	if got := int4Ptr(nil); got.Valid {
		t.Errorf("int4Ptr(nil): got Valid=%v, want Valid=false (NULL)", got.Valid)
	}
	if got := int4Ptr(ptr(int32(0))); !got.Valid || got.Int32 != 0 {
		t.Errorf("int4Ptr(&0): got {Valid=%v Int32=%d}, want {Valid=true Int32=0} — int4Ptr does NOT clamp", got.Valid, got.Int32)
	}
	if got := int4Ptr(ptr(int32(7))); !got.Valid || got.Int32 != 7 {
		t.Errorf("int4Ptr(&7): got {Valid=%v Int32=%d}, want {Valid=true Int32=7}", got.Valid, got.Int32)
	}
}
