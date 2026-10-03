-- =============================================================================
-- 0020_canonical_id_bit_layout_parity.sql
-- G1 - Domain Foundation: align the SQL canonical identifier encoder with the
--        Go one, so the two sides of the language boundary produce the same
--        value rather than merely the same shape.
--
-- Authority: 25_PRODUCT_BLUEPRINT_DOMAIN_PROFILE.md (canonical identity),
--            01_SYSTEM_ARCHITECTURE.md (Go is the control-plane authority).
--
-- WHY THIS MIGRATION EXISTS
--
-- 0013 introduced common.new_canonical_id as "the SQL twin of contracts.NewID".
-- It was not a twin. The two implementations encoded the same entropy
-- differently:
--
--   Go  (contracts/id.go, encodeCrockford)
--       16 bytes -> big-endian bit stream -> first 100 bits, 5 at a time,
--       most-significant bit of each group first.
--
--   SQL (0013, as written)
--       20 bytes -> per byte, get_byte(b,i)/8 -> top 5 bits of byte i.
--
-- Both emit 20 lowercase Crockford characters with 100 bits of entropy, so both
-- satisfy common.is_canonical_id and both pass every format test. Neither
-- produced the other's values. An identifier minted by a PostgreSQL DEFAULT was
-- not the identifier Go would have produced from the same 100 bits, and nothing
-- in the repository detected this, because no test compared the two encoders.
--
-- The G1 gate report recorded this as the reason canonical IDs could not be
-- called "implemented and verified" across the language boundary. It is a real
-- gap rather than a cosmetic one: the whole audit chain is keyed by identifiers
-- that both languages mint, and a divergence there means an identifier's
-- provenance cannot be reconstructed from its bytes.
--
-- WHAT THIS DOES AND DOES NOT CHANGE
--
-- It changes the mapping from entropy to characters. It does not change the
-- alphabet, the length, the prefix scheme, the entropy, or is_canonical_id, and
-- it retroactively invalidates nothing: identifiers minted before this migration
-- were unique and correctly formatted, and remain so. They simply no longer
-- correspond to any Go encoding, which was already true and is not made worse.
--
-- Rewriting history is not attempted and would be wrong. A migration that tried
-- to re-encode existing rows would have to touch the append-only audit chain.
-- =============================================================================

-- The encoder, written to mirror contracts.encodeCrockford line for line:
-- expand the bytes into a most-significant-bit-first stream, then take five
-- bits at a time. Written this way so that the parity is readable by comparing
-- the two implementations, rather than being a numerical coincidence that
-- happens to agree on one test vector.
CREATE OR REPLACE FUNCTION common.canonical_id_from_bytes(
    p_prefix TEXT,
    p_bytes  BYTEA
)
RETURNS TEXT
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    v_id TEXT;
BEGIN
    -- The entropy-width guard lives inside this function rather than beside it.
    -- It was originally a standalone common.assert_canonical_entropy that
    -- nothing called -- a control that exists and is never invoked, which is
    -- the same shape as defect 15, and was caught by mutation test C of
    -- db/mutate_0020_0021.ps1 in exactly the way it should be. A guard in a
    -- migration file that no code path reaches is documentation.
    PERFORM common.assert_canonical_entropy(p_bytes);

    WITH bits AS (
        SELECT (get_byte(p_bytes, i) >> (7 - j)) & 1 AS bit,
               i * 8 + j                             AS pos
          FROM generate_series(0, octet_length(p_bytes) - 1) AS i,
               generate_series(0, 7)                 AS j
         WHERE octet_length(p_bytes) > i
    )
    SELECT p_prefix || '_' || coalesce(string_agg(
               substr('0123456789abcdefghjkmnpqrstvwxyz',
                      (SELECT coalesce(sum(bits.bit::int << (4 - (bits.pos - c * 5))), 0)::int
                         FROM bits
                        WHERE bits.pos >= c * 5
                          AND bits.pos <  c * 5 + 5)
                      + 1, 1),
               '' ORDER BY c), '')
      INTO v_id
      FROM generate_series(0, 19) AS c;

    RETURN v_id;
END;
$$;

COMMENT ON FUNCTION common.canonical_id_from_bytes IS
    'Renders entropy as a canonical identifier without generating any: typed prefix plus 20 lowercase Crockford Base32 characters. Byte-for-byte identical to contracts.IDFromBytes over contracts.encodeCrockford, which takes the first 100 bits of a 16-byte big-endian bit stream, five at a time, most significant first. IMMUTABLE and STRICT so it can be used in an index expression and a generated column. The cross-language parity test pins the two implementations against each other.';


-- Exact entropy width. A caller that believes it has 128 bits of entropy and
-- supplies fewer has a bug that would otherwise be silently absorbed by
-- zero-padding, and a caller supplying more would have its surplus discarded
-- without knowing. Both are refused, which is the same contract
-- contracts.IDFromBytes enforces.
CREATE OR REPLACE FUNCTION common.new_canonical_id(p_prefix TEXT)
RETURNS TEXT
LANGUAGE sql
VOLATILE
PARALLEL SAFE
AS $$
    SELECT common.canonical_id_from_bytes(p_prefix, gen_random_bytes(16));
$$;

COMMENT ON FUNCTION common.new_canonical_id IS
    'Mints a canonical identifier: typed prefix plus 20 lowercase Crockford Base32 characters carrying 100 bits of entropy from gen_random_bytes(16). Bit-for-bit identical to contracts.NewID, which is the property 0013 claimed and did not deliver. 16 bytes rather than 13 because 128 bits is a whole number of bytes and the 28 surplus bits are discarded from uniform randomness without bias.';


-- The entropy width is now part of the contract, so it is enforced where the
-- contract lives rather than only described in a comment.
CREATE OR REPLACE FUNCTION common.assert_canonical_entropy(
    p_bytes BYTEA
)
RETURNS VOID
LANGUAGE plpgsql
IMMUTABLE
AS $$
BEGIN
    IF octet_length(p_bytes) <> 16 THEN
        RAISE EXCEPTION
            'canonical identifier entropy is % bytes, want exactly 16',
            octet_length(p_bytes)
            USING ERRCODE = 'check_violation',
                  HINT = '16 bytes is 128 bits; 100 are used and 28 are discarded';
    END IF;
END;
$$;
