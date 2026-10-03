package dbtest_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/aitc/trade/contracts"
	"github.com/aitc/trade/dbtest"
)

// Audit canonical-hash conformance and tamper-detection tests.
//
// Authority: 22_AUDIT_INTEGRITY_AND_EVIDENCE.md §3
//
//	record_hash = SHA-256(canonical_json(record_without_record_hash_and_signature));
//	previous_hash references the prior record hash in that partition.
//	Canonical serialization and schema version are fixed and tested.
//
// Two independent implementations of that serialisation exist:
// audit.canonical_payload in db/migrations/0009_audit_canonical_hash.sql, and
// contracts.CanonicalAuditPayload in contracts/audit.go. §3 requires the
// serialisation to be "fixed and tested"; the strongest available test of that
// is to require the two to agree byte-for-byte, so
// TestCanonicalAuditPayloadMatchesDatabaseImplementation does exactly that by
// calling the SQL function and comparing the returned string.
//
// The rest of this file proves the hash actually constrains something: that a
// caller cannot choose its own hash, and that a verifier recomputes rather than
// trusts.

// auditConformanceCase is one serialisation input. The Cases below are chosen to
// exercise the parts of the form that are easy to get wrong, not just the happy
// path: absent optional members (NULL, not ""), a reason containing characters
// that two different JSON encoders disagree about, and negative sequence numbers
// so a sign error cannot pass unnoticed.
type auditConformanceCase struct {
	name string
	rec  contracts.AuditRecord
}

func strptr(s string) *string { return &s }

func conformanceCases() []auditConformanceCase {
	fx := contracts.MarketFX
	eq := contracts.MarketEquities
	reasonWithMarkup := "limit exceeded <script> & \"quoted\" \\ backslash"

	return []auditConformanceCase{
		{
			name: "every optional member present",
			rec: contracts.AuditRecord{
				AuditID: "aud_01hq0000000000000000000001", PartitionKey: "2026-09",
				Sequence: 1, TenantOrOwnerScope: "owner-1",
				ActorID: "act_01hq0000000000000000000002", ActorType: contracts.ActorHuman,
				Action: "order.accepted", TargetType: "order",
				TargetID:               "ord_01hq0000000000000000000003",
				Environment:            contracts.EnvPaper,
				MarketScope:            &fx,
				OccurredAtUnixNano:     1788000000123456789,
				RecordedAtUnixNano:     1788000000123999888,
				Reason:                 strptr("within risk appetite"),
				CorrelationID:          "cor_01hq0000000000000000000004",
				CausationID:            strptr("cmd_01hq0000000000000000000005"),
				PolicyVersion:          "1.4.2",
				Result:                 contracts.AuditResultSuccess,
				BeforeDigest:           strptr("aa11bb22"),
				AfterDigest:            strptr("cc33dd44"),
				DetailsDigest:          "1111111111111111111111111111111111111111111111111111111111111111",
				PreviousHash:           contracts.AuditGenesisHash,
				CanonicalSchemaVersion: contracts.AuditCanonicalSchemaVersion,
			},
		},
		{
			name: "every optional member NULL",
			rec: contracts.AuditRecord{
				AuditID: "aud_01hq0000000000000000000006", PartitionKey: "2026-09",
				Sequence: 2, TenantOrOwnerScope: "owner-1",
				ActorID: "act_system", ActorType: contracts.ActorSystem,
				Action: "auth.denied", TargetType: "session",
				TargetID:               "ses_01hq0000000000000000000007",
				Environment:            contracts.EnvPaper,
				OccurredAtUnixNano:     1788000001123456789,
				RecordedAtUnixNano:     1788000001123999888,
				CorrelationID:          "cor_01hq0000000000000000000008",
				PolicyVersion:          "1.4.2",
				Result:                 contracts.AuditResultDenied,
				DetailsDigest:          "2222222222222222222222222222222222222222222222222222222222222222",
				PreviousHash:           "3333333333333333333333333333333333333333333333333333333333333333",
				CanonicalSchemaVersion: contracts.AuditCanonicalSchemaVersion,
			},
		},
		{
			// A non-market event has no market scope. Encoding that as an empty
			// string instead of null would conflate "not applicable" with a
			// malformed value, so the NULL path must serialise as JSON null.
			name: "market scope present, reason with markup characters",
			rec: contracts.AuditRecord{
				AuditID: "aud_01hq0000000000000000000009", PartitionKey: "2026-09",
				Sequence: 3, TenantOrOwnerScope: "owner-1",
				ActorID: "act_agent", ActorType: contracts.ActorAgent,
				Action: "risk.limit_breach", TargetType: "strategy",
				TargetID:               "str_01hq0000000000000000000010",
				Environment:            contracts.EnvDev,
				MarketScope:            &eq,
				OccurredAtUnixNano:     1788000002123456789,
				RecordedAtUnixNano:     1788000002123999888,
				Reason:                 strptr(reasonWithMarkup),
				CorrelationID:          "cor_01hq0000000000000000000011",
				PolicyVersion:          "1.4.2",
				Result:                 contracts.AuditResultFailure,
				DetailsDigest:          "4444444444444444444444444444444444444444444444444444444444444444",
				PreviousHash:           "5555555555555555555555555555555555555555555555555555555555555555",
				CanonicalSchemaVersion: contracts.AuditCanonicalSchemaVersion,
			},
		},
		{
			// A negative sequence and a zero timestamp catch a missing sign or a
			// unit mix-up (nanoseconds vs milliseconds) in the integer members.
			name: "negative sequence and zero nanosecond timestamps",
			rec: contracts.AuditRecord{
				AuditID: "aud_01hq0000000000000000000012", PartitionKey: "2026-09",
				Sequence: -1, TenantOrOwnerScope: "owner-1",
				ActorID: "act_workload", ActorType: contracts.ActorWorkload,
				Action: "test.edge", TargetType: "none",
				TargetID:               "none",
				Environment:            contracts.EnvTest,
				OccurredAtUnixNano:     0,
				RecordedAtUnixNano:     0,
				CorrelationID:          "cor_01hq0000000000000000000013",
				PolicyVersion:          "1.4.2",
				Result:                 contracts.AuditResultUnknown,
				DetailsDigest:          "6666666666666666666666666666666666666666666666666666666666666666",
				PreviousHash:           "7777777777777777777777777777777777777777777777777777777777777777",
				CanonicalSchemaVersion: contracts.AuditCanonicalSchemaVersion,
			},
		},
	}
}

// TestCanonicalAuditPayloadMatchesDatabaseImplementation requires the Go and SQL
// serialisations to be byte-identical. §3 fixes the serialisation; this is the
// test that keeps it fixed across the language boundary, and it is the check
// that would catch a change to the member order, a missing NULL case, or an
// encoder difference in escaping.
func TestCanonicalAuditPayloadMatchesDatabaseImplementation(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		for _, tc := range conformanceCases() {
			t.Run(tc.name, func(t *testing.T) {
				r := tc.rec

				var sqlPayload string
				dbtest.MustQueryRow(t, ctx, tx, &sqlPayload, `
                    SELECT audit.canonical_payload(
                             $1::text, $2::text, $3::bigint, $4::text, $5::text,
                             $6::audit.actor_type_v, $7::text, $8::text, $9::text,
                             $10::common.environment, $11::common.market_class,
                             $12::bigint, $13::bigint, $14::text, $15::text, $16::text,
                             $17::text, $18::audit.result_value, $19::text, $20::text,
                             '{"k":"v"}'::jsonb, $21::text, $22::text)`,
					r.AuditID, r.PartitionKey, r.Sequence, r.TenantOrOwnerScope,
					r.ActorID, string(r.ActorType), r.Action, r.TargetType, r.TargetID,
					string(r.Environment), marketScopeArg(r.MarketScope),
					r.OccurredAtUnixNano, r.RecordedAtUnixNano, r.Reason, r.CorrelationID,
					r.CausationID, r.PolicyVersion, string(r.Result), r.BeforeDigest,
					r.AfterDigest, r.PreviousHash, r.CanonicalSchemaVersion)

				// The database derives details_digest from the details object; the
				// Go payload carries that digest, not the object. Feed the
				// database's own digest to Go so the comparison isolates the
				// serialisation rather than the details hashing.
				var sqlDetailsDigest string
				dbtest.MustQueryRow(t, ctx, tx, &sqlDetailsDigest,
					`SELECT encode(public.digest('{"k":"v"}'::jsonb::text, 'sha256'), 'hex')`)
				r.DetailsDigest = sqlDetailsDigest

				goPayload := contracts.CanonicalAuditPayload(r)
				if goPayload != sqlPayload {
					t.Fatalf("canonical serialisation differs between Go and SQL\n  go:  %s\n  sql: %s", goPayload, sqlPayload)
				}

				// The hash must change when any hashed member changes, or the
				// "hash" is decorative.
				before, err := contracts.AuditRecordHash(r)
				if err != nil {
					t.Fatalf("hash: %v", err)
				}
				mutated := r
				mutated.Action = r.Action + ".mutated"
				after, err := contracts.AuditRecordHash(mutated)
				if err != nil {
					t.Fatalf("hash: %v", err)
				}
				if before == after {
					t.Fatal("changing the action did not change the record hash")
				}
			})
		}
	})
}

// TestAuditAppendAcceptsCorrectSuppliedHash proves the cross-check is not simply
// refusing everything: a hash computed independently by the Go implementation is
// accepted, and the value stored is that hash.
func TestAuditAppendAcceptsCorrectSuppliedHash(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		auditID := dbtest.CanonicalID("aud", 901)
		occurred := dbtest.NowNs()
		details := `{"reason":"conformance"}`

		// Obtain the details digest and the database's expected hash for the
		// exact record that will be appended, then append with that hash
		// supplied as the cross-check value.
		var expected string
		dbtest.MustQueryRow(t, ctx, tx, &expected, `
            SELECT audit.compute_record_hash(
                     $1::text, $2::text, 1, 'owner-1', $3::text, 'SYSTEM'::audit.actor_type_v,
                     'test.accepted', 'order', $4::text, 'paper'::common.environment,
                     NULL::common.market_class, $5::bigint, $5::bigint, 'because'::text,
                     'cor-1'::text, NULL::text, 'v1'::text, 'SUCCESS'::audit.result_value,
                     NULL::text, NULL::text, $6::jsonb,
                     audit.genesis_hash(), '1.0.0'::text)`,
			auditID, part, "actor-1", dbtest.CanonicalID("ord", 902), occurred, details)

		if _, err := tx.ExecContext(ctx, `
            SELECT audit.append_record($1,$2,1,'owner-1','actor-1','SYSTEM','test.accepted',
                'order',$3,'paper',NULL,common.ns_to_timestamptz($4),$4,common.ns_to_timestamptz($4),$4,'because','cor-1',NULL,'v1','SUCCESS',
                NULL,NULL,$5::jsonb,'key-1','1.0.0',$6)`,
			auditID, part, dbtest.CanonicalID("ord", 902), occurred, details, expected); err != nil {
			t.Fatalf("a correctly computed hash was rejected: %v", err)
		}

		var stored string
		dbtest.MustQueryRow(t, ctx, tx, &stored,
			`SELECT record_hash FROM audit.record WHERE audit_id = $1`, auditID)
		if stored != expected {
			t.Fatalf("stored record_hash = %q, want the cross-checked %q", stored, expected)
		}
	})
}

// TestAuditAppendRejectsDisagreeingSuppliedHash is the control that the whole
// migration exists for. Before 0009, a caller-supplied hash was stored verbatim,
// so an audited party could record any 64 hex characters it liked and the chain
// would link to them. A well-formed but wrong hash must now be refused.
func TestAuditAppendRejectsDisagreeingSuppliedHash(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		// Well-formed: 64 lowercase hex characters, so nothing about the value's
		// shape gives the forgery away. Only recomputation catches it.
		forged := strings.Repeat("a", 64)
		err := dbtest.ExpectRejected(t, ctx, tx, `
            SELECT audit.append_record($1,$2,1,'owner-1','actor-1','SYSTEM','test.forged',
                'order',$3,'paper',NULL,common.ns_to_timestamptz($4),$4,common.ns_to_timestamptz($4),$4,'because','cor-1',NULL,'v1','SUCCESS',
                NULL,NULL,'{}'::jsonb,'key-1','1.0.0',$5)`,
			dbtest.CanonicalID("aud", 911), part, dbtest.CanonicalID("ord", 912),
			dbtest.NowNs(), forged)
		if !strings.Contains(err.Error(), "record hash mismatch") {
			t.Fatalf("a forged record hash was not rejected as a hash mismatch: %v", err)
		}

		// Nothing may have been written by the rejected attempt.
		var count int
		dbtest.MustQueryRow(t, ctx, tx, &count,
			`SELECT count(*) FROM audit.record WHERE partition_key = $1`, part)
		if count != 0 {
			t.Fatalf("the rejected append left %d record(s) behind; a failed append must be atomic", count)
		}
	})
}

// TestAuditVerifyPartitionRecomputesRatherThanTrusts proves the verifier
// recomputes. It writes an intact chain and requires every row to verify, which
// would fail if verify_partition were merely reporting the stored hash back.
//
// The record's immutability trigger blocks UPDATE, so the tampering is performed
// with session_replication_role = 'replica', which disables triggers. That
// simulates the only realistic way evidence is altered: a privileged operator
// with direct table access, defeating the application-level controls. It is
// exactly the scenario the chain exists to make detectable, so the verifier must
// catch it.
func TestAuditVerifyPartitionRecomputesRatherThanTrusts(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		MustAppendAudit(t, ctx, tx, part, 1)
		MustAppendAudit(t, ctx, tx, part, 2)
		MustAppendAudit(t, ctx, tx, part, 3)

		assertChainVerifies(t, ctx, tx, part, 3)

		// Alter the content of record 2 without changing its stored hash.
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE audit.record SET action = 'tampered' WHERE partition_key = $1 AND sequence = 2`,
			part); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'origin'`); err != nil {
			t.Fatalf("restore triggers: %v", err)
		}

		// Recomputation must now fail for that record and only that record.
		var failed int
		dbtest.MustQueryRow(t, ctx, tx, &failed, `
            SELECT count(*) FILTER (WHERE NOT ok) FROM audit.verify_partition($1)`, part)
		if failed != 1 {
			t.Fatalf("verify_partition reported %d failed rows after tampering with one record, want 1", failed)
		}
	})
}

// TestAuditVerifyPartitionHeadDetectsTailTruncation covers the gap that
// verify_partition cannot: deleting the most recent record leaves every remaining
// record correctly chained, so only a comparison against the partition head
// reveals that evidence was removed.
func TestAuditVerifyPartitionHeadDetectsTailTruncation(t *testing.T) {
	db := dbtest.Open(t)
	dbtest.Resettable(t, db, func(ctx context.Context, tx *sql.Tx) {
		part := MustOpenPartition(t, ctx, tx)
		for seq := 1; seq <= 3; seq++ {
			MustAppendAudit(t, ctx, tx, part, seq)
		}

		var ok bool
		dbtest.MustQueryRow(t, ctx, tx, &ok, `SELECT ok FROM audit.verify_partition_head($1)`, part)
		if !ok {
			t.Fatal("the head did not match the records on an intact chain")
		}

		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM audit.record WHERE partition_key = $1 AND sequence = 3`, part); err != nil {
			t.Fatalf("truncate tail: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = 'origin'`); err != nil {
			t.Fatalf("restore triggers: %v", err)
		}

		// The remaining chain still verifies -- which is precisely why the head
		// comparison is a separate control.
		assertChainVerifies(t, ctx, tx, part, 2)

		dbtest.MustQueryRow(t, ctx, tx, &ok, `SELECT ok FROM audit.verify_partition_head($1)`, part)
		if ok {
			t.Fatal("removing the most recent record was not detected; the head still matched")
		}
	})
}

// assertChainVerifies requires every record in the partition to pass
// recomputation and linkage.
func assertChainVerifies(t *testing.T, ctx context.Context, tx *sql.Tx, part string, wantRows int) {
	t.Helper()
	var total int
	dbtest.MustQueryRow(t, ctx, tx, &total, `SELECT count(*) FROM audit.verify_partition($1)`, part)
	if total != wantRows {
		t.Fatalf("verify_partition returned %d rows, want %d", total, wantRows)
	}
	var failed int
	dbtest.MustQueryRow(t, ctx, tx, &failed,
		`SELECT count(*) FILTER (WHERE NOT ok) FROM audit.verify_partition($1)`, part)
	if failed != 0 {
		t.Fatalf("%d record(s) failed verification, want 0", failed)
	}
}

// marketScopeArg renders a nullable market class for a query argument.
func marketScopeArg(m *contracts.MarketClass) any {
	if m == nil {
		return nil
	}
	return string(*m)
}
