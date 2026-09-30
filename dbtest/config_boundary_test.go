package dbtest_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Tests for migration 0017, the configuration-boundary controls.
//
// Every test here is paired. A control that has only a negative test proves it
// can refuse; only a negative test cannot prove it can still be used, and a
// control that refuses everything is an outage rather than a control. The pairs
// are what make both halves credible.

const insertRevision = `
INSERT INTO config.revision (
    revision_id, environment, revision_number, schema_version,
    content_digest, signature, signing_key_id, document, status,
    created_at_ns, created_by, reason
) VALUES ($1, $2::common.environment, $3, '1.0.0',
          config.document_digest($4::jsonb), NULL, NULL, $4::jsonb, 'DRAFT',
          $5, $6, $7)`

// MustInsertRevision inserts a DRAFT revision whose content_digest is computed
// from its document, which is the honest way to author one. Tests that want a
// mismatched digest override the column after building the INSERT.
func MustInsertRevision(t *testing.T, ctx context.Context, tx *sql.Tx, env string, number int64, document string, by, reason string) string {
	t.Helper()
	id := dbtest.CanonicalID("cfg", int(number)*100+int(env[0]))
	dbtest.MustExec(t, ctx, tx, insertRevision,
		id, env, number, document, dbtest.NowNs(), by, reason)
	return id
}

// A configuration document that contains no secrets and satisfies nothing else
// either, so a test failure is unambiguously about the rule under test.
const cleanDocument = `{"schema":"1.0.0","features":{"a":true},"limits":{"max_orders":10}}`

// ---------------------------------------------------------------------------
// Control 1/2: content_digest must bind to the document.
// ---------------------------------------------------------------------------

// TestAConfigRevisionIsAccepted proves the digest binding is satisfiable. The
// digest is computed by config.document_digest, which is the only definition of
// the digest and the one the Go side must call rather than reimplement.
func TestAConfigRevisionIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")

		var digest string
		if err := tx.QueryRowContext(ctx,
			`SELECT content_digest FROM config.revision WHERE revision_id = $1`, id).Scan(&digest); err != nil {
			t.Fatalf("read back: %v", err)
		}
		var want string
		if err := tx.QueryRowContext(ctx,
			`SELECT config.document_digest($1::jsonb)`, cleanDocument).Scan(&want); err != nil {
			t.Fatalf("recompute digest: %v", err)
		}
		if digest != want {
			t.Fatalf("stored digest %s does not match the document digest %s", digest, want)
		}
	})
}

// TestAConfigDigestThatDisagreesWithItsDocumentIsRejected is the control that
// makes the signature mean something.
//
// signature is defined as a signature over content_digest. If the digest is not
// bound to the document, an operator can activate a snapshot with a genuine
// signature over an honest digest and then replace the document, leaving the
// signature and digest in place. The signature still verifies, the approval
// still stands, and the configuration is entirely different from the one that
// was approved. This is the failure mode a release signature exists to prevent.
func TestAConfigDigestThatDisagreesWithItsDocumentIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// A well-formed 64-char hex digest that describes a different document.
		disagreeing := fmt.Sprintf("%064x", 1)
		dbtest.ExpectRejectedBecause(t, ctx, tx, "content_digest", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason
			) VALUES ($1, 'paper', 1, '1.0.0', $2, $3::jsonb, 'DRAFT', $4, 'usr_owner', 'mismatched')`,
			dbtest.CanonicalID("cfg", 991), disagreeing, cleanDocument, dbtest.NowNs())
	})
}

// ---------------------------------------------------------------------------
// Control 3: content immutability and closed lifecycle.
// ---------------------------------------------------------------------------

// TestAnActiveConfigRevisionCannotBeEditedInPlace is the control 0001's
// comment claimed and never had. A snapshot that can be rewritten after it is
// loaded is not a snapshot.
func TestAnActiveConfigRevisionCannotBeEditedInPlace(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, id)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "immutable",
			`UPDATE config.revision SET document = '{"schema":"1.0.0","features":{"a":false},"limits":{"max_orders":9999}}'::jsonb
			  WHERE revision_id = $1`, id)
	})
}

// TestADraftConfigRevisionCannotBeEditedInPlace is the less obvious half.
// Allowing a DRAFT to be edited would mean the thing that was reviewed is not
// the thing that gets activated, because the artifact under review would have
// no stable identity. A change is a new revision, at every status.
func TestADraftConfigRevisionCannotBeEditedInPlace(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")

		dbtest.ExpectRejectedBecause(t, ctx, tx, "immutable",
			`UPDATE config.revision SET reason = 'quietly rewritten' WHERE revision_id = $1`, id)
	})
}

// TestAConfigRevisionCanStillBeActivated proves control 3 is a lifecycle guard
// and not a blanket refusal. The columns it permits to move are exactly the
// lifecycle columns.
func TestAConfigRevisionCanStillBeActivated(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")

		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, id)

		var status string
		var activated sql.NullTime
		if err := tx.QueryRowContext(ctx,
			`SELECT status, activated_at FROM config.revision WHERE revision_id = $1`, id).Scan(&status, &activated); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if status != "ACTIVE" || !activated.Valid {
			t.Fatalf("expected an ACTIVE revision with an activation timestamp, got status=%q activated=%v", status, activated.Valid)
		}
	})
}

// TestAnActivationTimestampCannotBeForged checks that the transition timestamp
// comes from the database. A caller-supplied activated_at would let a
// backdated activation masquerade as a prompt one during a later review.
func TestAnActivationTimestampCannotBeForged(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		backdated := "2000-01-01T00:00:00Z"

		// Supplying it is ignored rather than refused, because the trigger
		// overwrites it. The assertion is that the stored value is not the
		// backdated one.
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE', activated_at = $2::timestamptz WHERE revision_id = $1`, id, backdated)

		var activated sql.NullTime
		if err := tx.QueryRowContext(ctx,
			`SELECT activated_at FROM config.revision WHERE revision_id = $1`, id).Scan(&activated); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !activated.Valid || activated.Time.Year() == 2000 {
			t.Fatalf("activation timestamp was taken from the caller: %v", activated)
		}
	})
}

// TestAnActivatedConfigRevisionCannotReturnToDraft keeps the lifecycle closed.
// A terminal state is terminal, and a revision cannot be re-opened for editing
// because editing is impossible anyway -- which is precisely why returning to
// DRAFT would be a way to launder a revocation.
func TestAnActivatedConfigRevisionCannotReturnToDraft(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, id)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot move from",
			`UPDATE config.revision SET status = 'DRAFT' WHERE revision_id = $1`, id)
	})
}

// TestARevokedConfigRevisionIsTerminal makes the same point one step further
// along, where it matters most: a revoked configuration must not be revivable.
func TestARevokedConfigRevisionIsTerminal(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'REVOKED' WHERE revision_id = $1`, id)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot move from",
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, id)
	})
}

// TestAnActivatedConfigRevisionCannotBeDeleted keeps release evidence from
// being removed. audit.record has an independent copy of everything; this table
// is the only record of what configuration was actually in force.
func TestAnActivatedConfigRevisionCannotBeDeleted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, id)

		dbtest.ExpectRejectedBecause(t, ctx, tx, "cannot be deleted", `DELETE FROM config.revision WHERE revision_id = $1`, id)
	})
}

// TestADraftConfigRevisionCanBeDiscarded is the other half. A draft that could
// never be deleted would accumulate forever, and an operator with a mistaken
// draft would have no way to clear it except by leaving it to be superseded by
// something meaningless.
func TestADraftConfigRevisionCanBeDiscarded(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		id := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "mistake")

		dbtest.MustExec(t, ctx, tx, `DELETE FROM config.revision WHERE revision_id = $1`, id)

		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM config.revision WHERE revision_id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("draft was not discarded; %d row(s) remain", count)
		}
	})
}

// ---------------------------------------------------------------------------
// Control 4: at most one ACTIVE revision per environment.
// ---------------------------------------------------------------------------

// TestASecondActiveRevisionInOneEnvironmentIsRejected is the control replacing
// 0001's index, which was unique on (environment, revision_number) and so
// permitted any number of ACTIVE revisions per environment. Two ACTIVE
// snapshots in one environment is the ambiguity that produces a last-write-wins
// reader, which doc 01 §8 prohibits for authoritative financial state.
func TestASecondActiveRevisionInOneEnvironmentIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		first := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, first)

		second := MustInsertRevision(t, ctx, tx, "paper", 2, cleanDocument, "usr_owner", "follow-up")
		// Asserted rather than merely attempted. A version of this test that
		// simply issued the second activation and let the transaction fail would
		// pass identically with the unique index absent, because the error would
		// be swallowed by the rollback at the end of the test.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "duplicate key",
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, second)
	})
}

// TestSupersedingARevisionFreesTheActiveSlot proves control 4 is not a
// permanent one-shot lock. Superseding is the normal way a configuration
// changes, and it has to work.
func TestSupersedingARevisionFreesTheActiveSlot(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		first := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, first)
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'SUPERSEDED' WHERE revision_id = $1`, first)

		second := MustInsertRevision(t, ctx, tx, "paper", 2, cleanDocument, "usr_owner", "next")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, second)

		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM config.revision WHERE environment = 'paper' AND status = 'ACTIVE'`).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly one ACTIVE revision in paper, found %d", count)
		}
	})
}

// TestActiveRevisionsInDifferentEnvironmentsCoexist keeps control 4 scoped to
// the environment. Refusing a simultaneous ACTIVE revision in paper and shadow
// would be a different, and wrong, control.
func TestActiveRevisionsInDifferentEnvironmentsCoexist(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		paper := MustInsertRevision(t, ctx, tx, "paper", 1, cleanDocument, "usr_owner", "initial")
		shadow := MustInsertRevision(t, ctx, tx, "shadow", 1, cleanDocument, "usr_owner", "initial")

		dbtest.MustExec(t, ctx, tx, `UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, paper)
		dbtest.MustExec(t, ctx, tx, `UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, shadow)
	})
}

// ---------------------------------------------------------------------------
// Control 5: no embedded secrets.
// ---------------------------------------------------------------------------

// TestAConfigWithAnEmbeddedSecretIsRejected covers the direct case: a password
// field holding a password. Doc 17 §1 requires secrets to be referenced by
// secret-store identifier and never embedded.
func TestAConfigWithAnEmbeddedSecretIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.ExpectRejectedBecause(t, ctx, tx, "embedded secret", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason
			) VALUES ($1, 'paper', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3, 'usr_owner', 'embedded')`,
			dbtest.CanonicalID("cfg", 992), `{"db_password":"hunter2"}`, dbtest.NowNs())
	})
}

// TestAConfigWithANestedEmbeddedSecretIsRejected proves the check walks the
// whole document rather than only the top level. Secrets arrive nested, and a
// top-level-only check is the obvious way to write one.
func TestAConfigWithANestedEmbeddedSecretIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.ExpectRejectedBecause(t, ctx, tx, "embedded secret", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason
			) VALUES ($1, 'paper', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3, 'usr_owner', 'nested')`,
			dbtest.CanonicalID("cfg", 993),
			`{"venues":[{"name":"binance","credentials":{"api_key":"AKIAEXAMPLE"}}]}`,
			dbtest.NowNs())
	})
}

// TestAConfigWithASecretBearingKeyInsideAnArrayIsRejected closes the last
// structural gap: jsonb_each on a scalar errors, so a naive walker handles
// objects and then stops at the first array. Secrets in a list of venue
// credentials is the realistic shape.
func TestAConfigWithASecretBearingKeyInsideAnArrayIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.ExpectRejectedBecause(t, ctx, tx, "embedded secret", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason
			) VALUES ($1, 'paper', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3, 'usr_owner', 'in array')`,
			dbtest.CanonicalID("cfg", 994),
			`{"backends":[{"id":"b1"},{"id":"b2","access_token":"plaintext"}]}`,
			dbtest.NowNs())
	})
}

// TestAConfigWithAnEmbeddedPrivateKeyIsRejected matches on content rather than
// key name, because a PEM block is a secret however it is filed -- including
// under a name that looks entirely innocent.
func TestAConfigWithAnEmbeddedPrivateKeyIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		pem := `{"signing":{"primary":"-----BEGIN RSA PRIVATE KEY-----\nMIIEow...\n-----END RSA PRIVATE KEY-----"}}`
		dbtest.ExpectRejectedBecause(t, ctx, tx, "private key", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason
			) VALUES ($1, 'paper', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3, 'usr_owner', 'pem')`,
			dbtest.CanonicalID("cfg", 995), pem, dbtest.NowNs())
	})
}

// TestAConfigReferencingASecretByIdentifierIsAccepted is the half that decides
// whether this control is usable. A configuration that can only be written by
// embedding secrets has pushed the problem somewhere less visible, and this
// control would be switched off within a day.
func TestAConfigReferencingASecretByIdentifierIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustInsertRevision(t, ctx, tx, "paper", 1, `{
			"db_password": "secrets://prod/venue/aitc/binance#api-key",
			"signing":     {"primary": "secrets://prod/signing/hsm#aitc-release"}
		}`, "usr_owner", "references only")
	})
}

// TestInnocentKeyNamesContainingSecretWordsAreAccepted guards the control
// against becoming a nuisance. A substring matcher for "token" would reject
// token_endpoint and token_ttl, and a control that refuses valid configuration
// gets disabled. The matcher is segment-anchored for exactly this reason.
func TestInnocentKeyNamesContainingSecretWordsAreAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		MustInsertRevision(t, ctx, tx, "paper", 1, `{
			"token_endpoint": "https://identity.example/token",
			"token_ttl_seconds": 3600,
			"auth_method": "workload_identity",
			"secret_ref": "secrets://prod/whatever",
			"key_id": "kms-key-1"
		}`, "usr_owner", "innocent names")
	})
}

// ---------------------------------------------------------------------------
// Control 6: promotion provenance; live is never an automatic copy.
// ---------------------------------------------------------------------------

// TestAPromotionIntoLiveWithoutASecondApproverIsRejected is the control doc 17
// §1 states in one sentence: "Live configuration cannot be copied automatically
// from lower environments."
//
// The mechanism is deliberately not a flag or an environment variable. An
// automated pipeline has exactly one identity, the service account that wrote
// the revision, so it cannot supply a second distinct approver and cannot
// commit. Making the rule structural means there is nothing to misconfigure.
func TestAPromotionIntoLiveWithoutASecondApproverIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		source := MustInsertRevision(t, ctx, tx, "shadow", 1, cleanDocument, "usr_owner", "shadow config")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, source)

		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    derived_from_revision_id, signature, signing_key_id
			) VALUES ($1, 'live', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'svc_promotion_bot', 'automatic copy of shadow config', $4, '\x0102'::bytea, 'kms_key_live')`,
			dbtest.CanonicalID("cfg", 996), cleanDocument, dbtest.NowNs(), source)

		// The insert itself SUCCEEDS: the promotion rule is a deferred
		// constraint trigger, because a source revision may be written and only
		// activated later in the same transaction. The refusal happens when the
		// transaction is asked to settle.
		dbtest.ExpectDeferredRejected(t, ctx, tx, "cannot be copied automatically")
	})
}

// TestAPromotionIntoLiveApproverMustDifferFromTheAuthor is the dual-control
// half. One identity approving its own copy is not dual control, and it is the
// exact shape an automated pipeline would take if it were allowed to name an
// approver at all.
func TestAPromotionIntoLiveApproverMustDifferFromTheAuthor(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		source := MustInsertRevision(t, ctx, tx, "shadow", 1, cleanDocument, "usr_owner", "shadow config")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, source)

		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    derived_from_revision_id, promotion_approved_by, signature, signing_key_id
			) VALUES ($1, 'live', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'svc_promotion_bot', 'self-approved copy', $4, 'svc_promotion_bot', '\x0102'::bytea, 'kms_key_live')`,
			dbtest.CanonicalID("cfg", 997), cleanDocument, dbtest.NowNs(), source)

		dbtest.ExpectDeferredRejected(t, ctx, tx, "two distinct identities")
	})
}

// TestADualControlledPromotionIntoLiveIsAccepted proves the control is usable:
// a human-gated promotion into live is exactly the intended path and must work.
func TestADualControlledPromotionIntoLiveIsAccepted(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		source := MustInsertRevision(t, ctx, tx, "shadow", 1, cleanDocument, "usr_owner", "shadow config")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, source)

		target := dbtest.CanonicalID("cfg", 998)
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    derived_from_revision_id, promotion_approved_by, signature, signing_key_id
			) VALUES ($1, 'live', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'usr_release_eng', 'reviewed promotion of shadow config', $4, 'usr_risk_officer', '\x0102'::bytea, 'kms_key_live')`,
			target, cleanDocument, dbtest.NowNs(), source)

		dbtest.SettleDeferred(t, ctx, tx)
	})
}

// TestAPromotionMustGoForwardOnTheLadder keeps the ladder monotonic. Copying a
// live configuration down into shadow would place a production configuration in
// an environment that is deliberately less protected, and a stale shadow
// configuration into staging would be a silent downgrade.
func TestAPromotionMustGoForwardOnTheLadder(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		source := MustInsertRevision(t, ctx, tx, "staging", 1, cleanDocument, "usr_owner", "staging config")
		dbtest.MustExec(t, ctx, tx,
			`UPDATE config.revision SET status = 'ACTIVE' WHERE revision_id = $1`, source)

		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    derived_from_revision_id
			) VALUES ($1, 'dev', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'usr_release_eng', 'downward copy', $4)`,
			dbtest.CanonicalID("cfg", 999), cleanDocument, dbtest.NowNs(), source)

		dbtest.ExpectDeferredRejected(t, ctx, tx, "not forward on the promotion ladder")
	})
}

// TestAPromotionFromANonexistentRevisionIsRejected stops a promotion from
// claiming provenance it does not have, which would otherwise let a revision
// sidestep the live dual-control rule by naming a source that never existed.
//
// The refusal actually comes from the FOREIGN KEY, not from
// config.guard_promotion_provenance. The key is an immediate constraint, so it
// fires at INSERT and the row never exists. The trigger's equivalent NOT FOUND
// branch is therefore defence in depth that is unreachable through this path --
// worth keeping, because the trigger also fires on UPDATE, but it is not what is
// being tested here and the test says so rather than implying otherwise.
func TestAPromotionFromANonexistentRevisionIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		// The signature and key are supplied so the immediate
		// live_requires_signature CHECK is satisfied. Without them the insert
		// would be refused for an unrelated reason before the foreign key was
		// ever reached, and the test would prove nothing.
		dbtest.ExpectRejectedBecause(t, ctx, tx, "foreign key", `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    derived_from_revision_id, signature, signing_key_id
			) VALUES ($1, 'live', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'usr_release_eng', 'phantom provenance', 'cfg_zzzzzzzzzzzzzzzzzzzzzzzz', '\x0102'::bytea, 'kms_key_live')`,
			dbtest.CanonicalID("cfg", 1000), cleanDocument, dbtest.NowNs())
	})
}

// TestARevisionAuthoredDirectlyInLiveIsAllowed is the other half, and it is
// load-bearing for honesty rather than convenience. Doc 17 §1 forbids automatic
// copying, not direct authorship; forcing every live revision to declare a
// fabricated parent would push operators to invent provenance, which is worse
// than having none.
func TestARevisionAuthoredDirectlyInLiveIsAllowed(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO config.revision (
			    revision_id, environment, revision_number, schema_version,
			    content_digest, document, status, created_at_ns, created_by, reason,
			    signature, signing_key_id
			) VALUES ($1, 'live', 1, '1.0.0', config.document_digest($2::jsonb), $2::jsonb, 'DRAFT', $3,
			          'usr_release_eng', 'authored directly for live', '\x0102'::bytea, 'kms_key_live')`,
			dbtest.CanonicalID("cfg", 1001), cleanDocument, dbtest.NowNs())
	})
}
