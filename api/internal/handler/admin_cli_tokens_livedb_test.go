package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/clitoken"
)

// The admin standing-credential inventory (GET /api/admin/cli-tokens).
//
// Live-DB and through the REAL h.Routes() router, for the same reason the ceiling
// tests next door are: the property is cross-USER visibility, and a fake store
// cannot exhibit it. A test that seeded one user would pass against a query still
// scoped to one user_id, which is the exact bug this feature exists to fix.
//
// Scoped to its own fixture throughout. The LiveDB packages share one database and
// fixtures accumulate, so any assertion on totals would pass or fail on what other
// tests left behind.

// adminTokenFixture is one seeded token plus what the response should say about it.
type adminTokenFixture struct {
	id      uuid.UUID
	token   string
	owner   uuid.UUID
	scope   string
	revoked bool
}

func TestAdminCLITokenInventoryIsFactoryWideAndLeaksNoCredentialLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	other := cliSeedUser(t, pool, false)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	past := time.Now().Add(-24 * time.Hour)
	fixtures := make([]adminTokenFixture, 0, 4)
	add := func(owner uuid.UUID, scope string, expires *time.Time, revoked bool) {
		tok, id := cliInsertToken(t, pool, owner, scope, expires, revoked)
		fixtures = append(fixtures, adminTokenFixture{id: id, token: tok, owner: owner, scope: scope, revoked: revoked})
	}
	// The never-expiring user-scope token is the row this feature most exists to
	// surface: nothing revokes it and nothing ages it out.
	add(other, clitoken.ScopeUser, nil, false)
	add(other, clitoken.ScopeUser, &past, false) // expired but not revoked
	add(other, clitoken.ScopeUser, nil, true)    // revoked: the incident trail
	add(admin, clitoken.ScopeAdminRO, nil, false)

	rec := bearerReq(router, http.MethodGet, "/api/admin/cli-tokens", uza)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/cli-tokens = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()

	// THE SECURITY ASSERTION, made on the RAW BYTES rather than on the DTO's shape.
	// A struct-field check would pass against a handler that re-marshalled the store
	// row; this cannot. Both encodings are covered: the plaintext token (were it ever
	// stored, which it is not) and the base64 of its sha256, which is what a []byte
	// token_hash marshals to if the projection ever regains the column.
	for _, f := range fixtures {
		if strings.Contains(raw, f.token) {
			t.Errorf("response body contains a PLAINTEXT CLI TOKEN (%s…)", f.token[:12])
		}
		b64 := base64.StdEncoding.EncodeToString(clitoken.Hash(f.token))
		if strings.Contains(raw, b64) {
			t.Errorf("response body contains the sha256 of a CLI token — the projection has regained token_hash, "+
				"which hands an admin an offline-crackable credential list (token %s…)", f.token[:12])
		}
	}
	for _, key := range []string{"token_hash", `"token"`} {
		if strings.Contains(raw, key) {
			t.Errorf("response body carries a %s key; the inventory is metadata-only", key)
		}
	}

	var body struct {
		Tokens []apitypes.AdminCLITokenDTO `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Index this fixture's rows by id, ignoring everything else in the shared table.
	pos := make(map[uuid.UUID]int, len(fixtures))
	got := make(map[uuid.UUID]apitypes.AdminCLITokenDTO, len(fixtures))
	for i, row := range body.Tokens {
		id, err := uuid.Parse(row.ID)
		if err != nil {
			t.Fatalf("row %d has a non-uuid id %q", i, row.ID)
		}
		for _, f := range fixtures {
			if f.id == id {
				pos[id] = i
				got[id] = row
			}
		}
	}

	// CROSS-USER VISIBILITY — the feature. Both owners' tokens reach one admin
	// response, which is precisely what every other query in cli_tokens.sql refuses
	// to do.
	if len(got) != len(fixtures) {
		t.Fatalf("inventory returned %d of this fixture's %d tokens; a per-user-scoped query would look exactly like this", len(got), len(fixtures))
	}
	owners := map[uuid.UUID]bool{}
	for _, f := range fixtures {
		row := got[f.id]
		owners[f.owner] = true
		if row.UserID != f.owner.String() {
			t.Errorf("token %s: user_id = %q, want %q", f.id, row.UserID, f.owner)
		}
		// THE JOIN, pinned to the OWNER rather than to the shape of an address.
		//
		// This assertion used to read `== "" || !strings.Contains(row.OwnerEmail, "@")`, and
		// that pinned nothing: cliSeedUser writes `cli-<uuid>@e2e` for EVERY user, so any
		// human's address satisfies a shape check. MEASURED at 5d5d0be4 — folding
		// `JOIN users u ON u.id = t.user_id` to `ON true` in cli_tokens.sql.go vetted clean and
		// this test still passed, while the same fold turned 10 rows into 40 and reported a
		// token under a DIFFERENT human's email. A cross-join is the failure this column exists
		// to make impossible, and the shape check could not see it.
		//
		// cliSeedUser builds the address deterministically from the user id, so the expected
		// value is derivable here without a second query — which is what makes this a pin on
		// the JOIN PREDICATE rather than on the presence of a column.
		wantEmail := fmt.Sprintf("cli-%s@e2e", f.owner)
		if row.OwnerEmail != wantEmail {
			t.Errorf("token %s: owner_email = %q, want %q — the users JOIN attributed this token to "+
				"the wrong human (or dropped its predicate); without a correct address an admin has "+
				"ids and cannot name anyone", f.id, row.OwnerEmail, wantEmail)
		}
		if row.Scope != f.scope {
			t.Errorf("token %s: scope = %q, want %q", f.id, row.Scope, f.scope)
		}
		if row.Revoked != f.revoked {
			t.Errorf("token %s: revoked = %v, want %v", f.id, row.Revoked, f.revoked)
		}
		if row.TokenPrefix == "" {
			t.Errorf("token %s: token_prefix is empty — it is the only way to name a row without revealing it", f.id)
		}
	}
	if len(owners) < 2 {
		t.Fatalf("fixture covers %d owners; the cross-user property needs at least 2", len(owners))
	}

	// ORDER: revoked rows sort last. Asserted as a property BETWEEN this fixture's own
	// rows, which holds whatever else the shared table contains — the global order puts
	// every un-revoked row ahead of every revoked one, so other fixtures may interleave
	// without affecting it.
	for _, active := range fixtures {
		if active.revoked {
			continue
		}
		for _, dead := range fixtures {
			if !dead.revoked {
				continue
			}
			if pos[active.id] > pos[dead.id] {
				t.Errorf("revoked token %s sorts before un-revoked %s (positions %d and %d); "+
					"active credentials must lead the inventory", dead.id, active.id, pos[dead.id], pos[active.id])
			}
		}
	}

	// The never-expiring token reports a null expiry rather than a zero time: it is the
	// row an operator most needs to spot, and a zero timestamp would render as an
	// ancient date and read as long-expired.
	for _, f := range fixtures {
		if f.scope == clitoken.ScopeUser && !f.revoked && got[f.id].ExpiresAt == nil {
			return
		}
	}
	t.Error("no fixture row came back with a null expires_at; the never-expiring user token is the one this view exists to surface")
}
