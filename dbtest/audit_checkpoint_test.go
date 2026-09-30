package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/dbtest"
)

// Checkpoint binding and key-binding tests.
//
// Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3 and §6.
//
// The checkpoint is the periodic, signed commitment to a batch of evidence. It
// is the artefact an auditor verifies against, and the reason a sealed batch
// stays tamper-evident after the source records have been altered.
//
// Two separate properties are under test, and conflating them would overstate
// what is proven:
//
//   Binding (0010). The checkpoint is bound to the records it names, and the
//   named sequence range is fully present. Proven by recomputation.
//
//   Key binding (0012). The checkpoint is attributable to a registered signing
//   key that was legitimate to sign with at the moment of sealing, and the
//   signature is of plausible size. Proven by the database refusing writes.
//
// NOT proven, and not claimed anywhere: that the signature bytes are a valid
// signature. That requires the KMS/HSM public key, and key material
// deliberately does not exist in this database.

// insertCheckpointAt inserts a checkpoint with every field under the caller's
// control, so each negative case differs from a valid checkpoint in exactly one
// respect. The signing key and signature are supplied by the caller because
// several tests vary them deliberately.
func insertCheckpointAt(t *testing.T, ctx context.Context, tx *sql.Tx, args ...any) error {
	t.Helper()
	_, err := tx.ExecContext(ctx, `
        INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
            record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns,
            expected_next_hash)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,decode($9,'hex'),$10,$11)`, args...)
	return err
}

// TestCheckpointMustBeBoundToTheRecordsItAttestsTo is the core negative test: a
// well-formed but unrelated hash must be refused. The value is 64 lowercase hex
// characters, so nothing about its shape gives the forgery away; only
// recomputation catches it.
func TestCheckpointMustBeBoundToTheRecordsItAttestsTo(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		MustRegisterSigningKey(t, ctx, tx)
		realHash := MustRecordHash(t, ctx, tx, part, 1)
		forged := strings.Repeat("a", 64)
		if forged == realHash {
			t.Fatal("the forged hash happens to equal the real one; the test proves nothing")
		}

		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,1,1,$3,$3,$4,decode($5,'hex'),$6)`,
			dbtest.CanonicalID("ckp", 501), part, forged, TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "not bound to the records it attests to") {
			t.Fatalf("an unbound checkpoint hash was not rejected as unbound: %v", err)
		}
	})
}

// TestCheckpointCannotAttestToRecordsThatDoNotExist closes the other direction:
// a checkpoint over correct-looking hashes for a sequence range that was never
// written. This is the "attest to evidence that does not exist" case, and it is
// distinct from the forgery case above.
func TestCheckpointCannotAttestToRecordsThatDoNotExist(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		MustRegisterSigningKey(t, ctx, tx)
		realHash := MustRecordHash(t, ctx, tx, part, 1)

		// Sequence 5 was never appended. The hashes are the genuine hash of
		// sequence 1, so only the existence check can catch this.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,5,5,1,$3,$3,$4,decode($5,'hex'),$6)`,
			dbtest.CanonicalID("ckp", 502), part, realHash, TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "no such record exists") {
			t.Fatalf("a checkpoint over a non-existent record was not rejected: %v", err)
		}
	})
}

// TestCheckpointSealingBeforeTheHeadMustNameItsSuccessor covers the truncated
// batch. A checkpoint sealed at a point that is not the current head has to
// name the successor's hash, otherwise deleting everything after the sealed
// range leaves a chain that still verifies and a checkpoint that still matches.
// expected_next_hash exists in the schema for exactly this and nothing enforced
// it before 0010.
func TestCheckpointSealingBeforeTheHeadMustNameItsSuccessor(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		for seq := 1; seq <= 3; seq++ {
			MustAppendAudit(t, ctx, tx, part, seq)
		}
		MustRegisterSigningKey(t, ctx, tx)
		firstHash := MustRecordHash(t, ctx, tx, part, 1)
		secondHash := MustRecordHash(t, ctx, tx, part, 2)
		thirdHash := MustRecordHash(t, ctx, tx, part, 3)

		// Sealing 1..2 while record 3 exists, with no successor named.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns,
                expected_next_hash)
            VALUES ($1,$2,1,2,2,$3,$4,$5,decode($6,'hex'),$7,NULL)`,
			dbtest.CanonicalID("ckp", 503), part, firstHash, secondHash,
			TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "names no expected_next_hash") {
			t.Fatalf("sealing before the head without naming a successor was not rejected: %v", err)
		}

		// Naming the wrong successor must also be refused.
		err = dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns,
                expected_next_hash)
            VALUES ($1,$2,1,2,2,$3,$4,$5,decode($6,'hex'),$7,$8)`,
			dbtest.CanonicalID("ckp", 504), part, firstHash, secondHash, TestSigningKeyID,
			testSignatureHex, dbtest.NowNs(), strings.Repeat("b", 64))
		if !strings.Contains(err.Error(), "expected_next_hash") {
			t.Fatalf("a wrong expected_next_hash was not rejected: %v", err)
		}

		// The correct successor is accepted, which is what makes the two
		// rejections above meaningful.
		if err := insertCheckpointAt(t, ctx, tx,
			dbtest.CanonicalID("ckp", 505), part, 1, 2, 2, firstHash, secondHash,
			TestSigningKeyID, testSignatureHex, dbtest.NowNs(), thirdHash); err != nil {
			t.Fatalf("a correctly bound mid-partition checkpoint was rejected: %v", err)
		}
	})
}

// TestCheckpointSealingAtTheHeadMustNotNameASuccessor is the converse. At the
// head there is no successor, so a checkpoint claiming one is asserting
// something about the future that cannot be verified and is a category error
// rather than a harmless default.
func TestCheckpointSealingAtTheHeadMustNotNameASuccessor(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		for seq := 1; seq <= 2; seq++ {
			MustAppendAudit(t, ctx, tx, part, seq)
		}
		MustRegisterSigningKey(t, ctx, tx)
		firstHash := MustRecordHash(t, ctx, tx, part, 1)
		secondHash := MustRecordHash(t, ctx, tx, part, 2)

		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns,
                expected_next_hash)
            VALUES ($1,$2,1,2,2,$3,$4,$5,decode($6,'hex'),$7,$8)`,
			dbtest.CanonicalID("ckp", 511), part, firstHash, secondHash, TestSigningKeyID,
			testSignatureHex, dbtest.NowNs(), strings.Repeat("c", 64))
		if !strings.Contains(err.Error(), "must be NULL") {
			t.Fatalf("sealing at the head while naming a successor was not rejected: %v", err)
		}
	})
}

// TestCheckpointSealedRangeMayNotContainGaps proves the sealed range is fully
// present, not just arithmetically consistent. The arithmetic CHECK
// (record_count = last - first + 1) is satisfied by a range whose middle
// records were deleted.
func TestCheckpointSealedRangeMayNotContainGaps(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		for seq := 1; seq <= 3; seq++ {
			MustAppendAudit(t, ctx, tx, part, seq)
		}
		MustRegisterSigningKey(t, ctx, tx)
		firstHash := MustRecordHash(t, ctx, tx, part, 1)
		lastHash := MustRecordHash(t, ctx, tx, part, 3)

		// Remove the middle record, as a privileged operator could.
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM audit.record WHERE partition_key = $1 AND sequence = 2`, part); err != nil {
			t.Fatalf("delete middle record: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'origin'`); err != nil {
			t.Fatalf("restore triggers: %v", err)
		}

		// record_count = 3 - 1 + 1 = 3 satisfies the arithmetic, but only two
		// records are present.
		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,3,3,$3,$4,$5,decode($6,'hex'),$7)`,
			dbtest.CanonicalID("ckp", 521), part, firstHash, lastHash,
			TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "must contain no gaps") {
			t.Fatalf("a sealed range with a gap was not rejected: %v", err)
		}
	})
}

// TestVerifyCheckpointsDetectEvidenceRewrittenAfterSigning is the end-to-end
// detection test. A correctly bound checkpoint is inserted and verified, then
// the record it names is rewritten by a privileged operator. The checkpoint
// itself is untouched and still internally well-formed, so nothing about its
// shape changes -- only audit.verify_checkpoints, which re-derives the claim
// from the records, can notice.
func TestVerifyCheckpointsDetectEvidenceRewrittenAfterSigning(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		for seq := 1; seq <= 3; seq++ {
			MustAppendAudit(t, ctx, tx, part, seq)
		}
		MustRegisterSigningKey(t, ctx, tx)
		// Sealing 1..2 is not the head, so the successor must be named. One
		// checkpoint covers the range; a second over the same range would be
		// refused by checkpoint_range_unique and is not what this test is about.
		if err := insertCheckpointAt(t, ctx, tx,
			dbtest.CanonicalID("ckp", 531), part, 1, 2, 2,
			MustRecordHash(t, ctx, tx, part, 1), MustRecordHash(t, ctx, tx, part, 2),
			TestSigningKeyID, testSignatureHex, dbtest.NowNs(),
			MustRecordHash(t, ctx, tx, part, 3)); err != nil {
			t.Fatalf("insert checkpoint: %v", err)
		}

		var failed int
		dbtest.MustQueryRow(t, ctx, tx, &failed,
			`SELECT count(*) FILTER (WHERE NOT ok) FROM audit.verify_checkpoints($1)`, part)
		if failed != 0 {
			t.Fatalf("%d checkpoint(s) failed verification on a freshly sealed chain", failed)
		}

		// Rewrite the content of a sealed record without touching the checkpoint.
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audit.record SET action = 'rewritten.after.signing'
              WHERE partition_key = $1 AND sequence = 2`, part); err != nil {
			t.Fatalf("rewrite record: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'origin'`); err != nil {
			t.Fatalf("restore triggers: %v", err)
		}

		dbtest.MustQueryRow(t, ctx, tx, &failed,
			`SELECT count(*) FILTER (WHERE NOT ok) FROM audit.verify_checkpoints($1)`, part)
		if failed != 1 {
			t.Fatalf("rewriting a sealed record left %d failing checkpoint(s), want 1", failed)
		}
	})
}

// ---------------------------------------------------------------------------
// Key binding (0012)
// ---------------------------------------------------------------------------

// TestCheckpointMustNameARegisteredSigningKey is the attribution control.
// audit.checkpoint.signing_key_id had no foreign key to audit.signing_key, so
// any string at all could be recorded as the key that sealed the batch.
func TestCheckpointMustNameARegisteredSigningKey(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		MustRegisterSigningKey(t, ctx, tx)
		hash := MustRecordHash(t, ctx, tx, part, 1)

		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,1,1,$3,$3,'key-that-does-not-exist',decode($4,'hex'),$5)`,
			dbtest.CanonicalID("ckp", 601), part, hash, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "not in the audit.signing_key registry") {
			t.Fatalf("an unregistered signing key was not rejected: %v", err)
		}
	})
}

// TestCompromisedOrRetiredKeyMayNotSignNewEvidence covers the key status
// machine. A key that is RETIRED or COMPROMISED must not seal a new batch; an
// ACTIVE or RETIRING key may.
func TestCompromisedOrRetiredKeyMayNotSignNewEvidence(t *testing.T) {
	db := dbtest.Open(t)
	for _, status := range []string{"RETIRED", "COMPROMISED"} {
		t.Run(status, func(t *testing.T) {
			dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
				part := MustOpenPartition(t, ctx, tx)
				MustAppendAudit(t, ctx, tx, part, seqOne)
				MustRegisterSigningKey(t, ctx, tx)
				if _, err := tx.ExecContext(ctx,
					`UPDATE audit.signing_key SET status = $2 WHERE signing_key_id = $1`,
					TestSigningKeyID, status); err != nil {
					t.Fatalf("set key status: %v", err)
				}
				hash := MustRecordHash(t, ctx, tx, part, seqOne)

				err := dbtest.ExpectRejected(t, ctx, tx, `
                    INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                        record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
                    VALUES ($1,$2,1,1,1,$3,$3,$4,decode($5,'hex'),$6)`,
					dbtest.CanonicalID("ckp", 611), part, hash, TestSigningKeyID,
					testSignatureHex, dbtest.NowNs())
				if !strings.Contains(err.Error(), "Only ACTIVE or RETIRING keys may sign") {
					t.Fatalf("a %s key was allowed to sign a new checkpoint: %v", status, err)
				}
			})
		})
	}
}

// seqOne keeps the literal 1 out of the call sites above.
const seqOne = 1

// TestOversdueSigningKeyMayNotSignNewEvidence covers rotation. A key past its
// rotate_after deadline may not seal new evidence: 22_..._EVIDENCE.md requires
// annual rotation, and an overdue key is one nobody has looked at in a year.
func TestOversdueSigningKeyMayNotSignNewEvidence(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, seqOne)
		MustRegisterSigningKey(t, ctx, tx)
		// Both dates move: signing_key_rotation_due requires
		// rotate_after > activated_at, so backdating only the deadline would be
		// refused by that constraint before the control under test could run.
		if _, err := tx.ExecContext(ctx,
			`UPDATE audit.signing_key SET activated_at = now() - interval '400 days',
                                       rotate_after = now() - interval '1 day'
              WHERE signing_key_id = $1`, TestSigningKeyID); err != nil {
			t.Fatalf("backdate rotation deadline: %v", err)
		}
		hash := MustRecordHash(t, ctx, tx, part, seqOne)

		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,1,1,$3,$3,$4,decode($5,'hex'),$6)`,
			dbtest.CanonicalID("ckp", 621), part, hash, TestSigningKeyID, testSignatureHex, dbtest.NowNs())
		if !strings.Contains(err.Error(), "passed its rotation deadline") {
			t.Fatalf("an overdue signing key was not rejected: %v", err)
		}
	})
}

// TestPlaceholderSignatureIsRejected is the control that makes the previous
// fixtures impossible to restore. The only check before 0012 was
// octet_length(signature) > 0, so decode('00','hex') -- one null byte -- was an
// acceptable signature, and it is what this suite was using. A 64-byte floor
// per algorithm rejects a placeholder without pretending to verify a real one.
func TestPlaceholderSignatureIsRejected(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, seqOne)
		MustRegisterSigningKey(t, ctx, tx)
		hash := MustRecordHash(t, ctx, tx, part, seqOne)

		err := dbtest.ExpectRejected(t, ctx, tx, `
            INSERT INTO audit.checkpoint (checkpoint_id, partition_key, first_sequence, last_sequence,
                record_count, first_hash, last_hash, signing_key_id, signature, signed_at_ns)
            VALUES ($1,$2,1,1,1,$3,$3,$4,decode('00','hex'),$5)`,
			dbtest.CanonicalID("ckp", 631), part, hash, TestSigningKeyID, dbtest.NowNs())
		if !strings.Contains(err.Error(), "is not a signature") {
			t.Fatalf("a one-byte placeholder signature was accepted: %v", err)
		}
	})
}

// TestCompromisedKeyIsRetrospectivelyReported is the alarm that matters after
// the fact. A key marked COMPROMISED today must surface every batch it signed,
// because those are exactly the batches whose authenticity is now in doubt.
// This is a report, not a refusal: the batches were legitimately sealed at the
// time, and deleting or hiding them would be worse than surfacing them.
func TestCompromisedKeyIsRetrospectivelyReported(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, seqOne)
		MustRegisterSigningKey(t, ctx, tx)
		MustInsertCheckpoint(t, ctx, tx, part, seqOne, seqOne)

		var compromised int
		dbtest.MustQueryRow(t, ctx, tx, &compromised, `
            SELECT count(*) FILTER (WHERE key_compromised) FROM audit.verify_checkpoint_key_binding($1)`, part)
		if compromised != 0 {
			t.Fatalf("%d checkpoint(s) flagged before the key was compromised", compromised)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE audit.signing_key SET status = 'COMPROMISED' WHERE signing_key_id = $1`,
			TestSigningKeyID); err != nil {
			t.Fatalf("compromise key: %v", err)
		}

		dbtest.MustQueryRow(t, ctx, tx, &compromised, `
            SELECT count(*) FILTER (WHERE key_compromised AND NOT ok) FROM audit.verify_checkpoint_key_binding($1)`, part)
		if compromised != 1 {
			t.Fatalf("compromising the key left %d failing checkpoint(s), want 1", compromised)
		}
	})
}
