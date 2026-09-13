package controller

// Property-based tests (stdlib testing/quick) for the audit hash chain
// (docs/adr/0004-audit-chain-format.md). The functions under test —
// AuditChainHash and VerifyAuditChain — are the pure chain primitives every
// store backend shares, so these tests live here rather than per-backend:
// the properties are over generated event sequences, asserting the honest
// chain always verifies and every tamper class the ADR claims to detect
// (mutation, reorder, interior deletion, insertion) always breaks
// verification at the right seq — plus the ADR's documented residual gap
// (tail truncation) is pinned as the SPEC, so a future "fix" that changes
// it is a deliberate, reviewed act.

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/bifrost-compute/bifrost/internal/core"
)

// genEvent is a quick.Generator over core.AuditEvent: every pointer field
// nil or set, both decisions, arbitrary timestamps — so the canonical-JSON
// normalization in AuditEvent.MarshalJSON (nil GrantedRoles -> [], empty
// Decision -> allow) is exercised across the space too.
type genEvent core.AuditEvent

var genStrings = []string{"", "alice", "bob", "create_cluster", "GET", "/api/v1/clusters", "quota_exceeded", "=evil", "\x00control"}

func genOptString(r *rand.Rand) *string {
	if r.Intn(2) == 0 {
		return nil
	}
	return strPtr(genStrings[r.Intn(len(genStrings))])
}

func (genEvent) Generate(r *rand.Rand, _ int) reflect.Value {
	e := core.AuditEvent{
		Ts:       r.Uint64(),
		Subject:  genOptString(r),
		Reason:   genOptString(r),
		Action:   genOptString(r),
		Cluster:  genOptString(r),
		Method:   genOptString(r),
		Path:     genOptString(r),
		Decision: core.AuditDecisionAllow,
	}
	if r.Intn(2) == 0 {
		e.Decision = core.AuditDecisionDeny
	}
	if r.Intn(2) == 0 {
		s := uint16(r.Intn(600))
		e.Status = &s
	}
	if r.Intn(2) == 0 {
		l := r.Uint64()
		e.LatencyMs = &l
	}
	if r.Intn(4) == 0 {
		e.Required = &core.AuditRequired{Action: "write", Target: "cluster"}
	}
	if n := r.Intn(3); n > 0 {
		e.GrantedRoles = make([]string, n)
		for i := range e.GrantedRoles {
			e.GrantedRoles[i] = genStrings[r.Intn(len(genStrings))]
		}
	}
	return reflect.ValueOf(genEvent(e))
}

func toEvents(gs []genEvent) []*core.AuditEvent {
	out := make([]*core.AuditEvent, len(gs))
	for i := range gs {
		e := core.AuditEvent(gs[i])
		out[i] = &e
	}
	return out
}

// The honest chain always verifies, end to end and from genesis, with
// EventsChecked accounting for every row. Empty and nil windows verify
// trivially (nothing to check) regardless of head.
func TestChainPropertyHonestChainAlwaysVerifies(t *testing.T) {
	f := func(gs []genEvent) bool {
		rows := chainRows(toEvents(gs))
		v := VerifyAuditChain(AuditGenesisHash, rows)
		return v.OK() && v.EventsChecked == uint64(len(rows)) && v.FirstBrokenSeq == nil
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
	v := VerifyAuditChain(AuditGenesisHash, nil)
	if !v.OK() || v.EventsChecked != 0 {
		t.Errorf("nil window: OK=%v checked=%d", v.OK(), v.EventsChecked)
	}
	v = VerifyAuditChain("anything", []ChainedAuditRow{})
	if !v.OK() || v.EventsChecked != 0 {
		t.Errorf("empty window: OK=%v checked=%d", v.OK(), v.EventsChecked)
	}
}

// mutateEvent applies one canonical-JSON-visible mutation chosen by pick.
// Every arm provably changes the marshaled bytes (a flipped Ts bit, a
// toggled decision, a nil->set/set-longer pointer, an appended role).
func mutateEvent(e *core.AuditEvent, pick uint8) {
	switch pick % 4 {
	case 0:
		e.Ts ^= 1 << (pick % 63)
	case 1:
		if e.Decision == core.AuditDecisionDeny {
			e.Decision = core.AuditDecisionAllow
		} else {
			e.Decision = core.AuditDecisionDeny
		}
	case 2:
		if e.Subject == nil {
			e.Subject = strPtr("mutated")
		} else {
			e.Subject = strPtr(*e.Subject + "x")
		}
	default:
		e.GrantedRoles = append(append([]string(nil), e.GrantedRoles...), "mutated-role")
	}
}

// Any single-event mutation — without recomputing every later hash —
// breaks verification AT the mutated row: EventsChecked counts exactly the
// rows before it and FirstBrokenSeq names its seq.
func TestChainPropertySingleMutationAlwaysBreaks(t *testing.T) {
	f := func(gs []genEvent, idx, pick uint8) bool {
		events := toEvents(gs)
		if len(events) == 0 {
			return true
		}
		rows := chainRows(events)
		i := int(idx) % len(rows)
		mutateEvent(&rows[i].Event, pick)
		v := VerifyAuditChain(AuditGenesisHash, rows)
		return !v.OK() &&
			v.EventsChecked == uint64(i) &&
			v.FirstBrokenSeq != nil && *v.FirstBrokenSeq == rows[i].Seq
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// Any swap of two rows breaks at the earlier swapped position: the row now
// there chains from a different predecessor than its stored hash encodes.
func TestChainPropertyReorderAlwaysBreaks(t *testing.T) {
	f := func(gs []genEvent, a, b uint8) bool {
		events := toEvents(gs)
		if len(events) < 2 {
			return true
		}
		rows := chainRows(events)
		i, j := int(a)%len(rows), int(b)%len(rows)
		if i == j {
			j = (i + 1) % len(rows)
		}
		if i > j {
			i, j = j, i
		}
		rows[i], rows[j] = rows[j], rows[i]
		v := VerifyAuditChain(AuditGenesisHash, rows)
		return !v.OK() &&
			v.EventsChecked == uint64(i) &&
			v.FirstBrokenSeq != nil && *v.FirstBrokenSeq == rows[i].Seq
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// Deleting an interior row breaks at the row after the gap: its stored
// hash chains from the deleted row, which the replay no longer knows.
func TestChainPropertyInteriorDeletionAlwaysBreaks(t *testing.T) {
	f := func(gs []genEvent, idx uint8) bool {
		events := toEvents(gs)
		if len(events) < 2 {
			return true
		}
		rows := chainRows(events)
		k := int(idx) % (len(rows) - 1) // any row except the last
		wantSeq := rows[k+1].Seq
		cut := append(append([]ChainedAuditRow{}, rows[:k]...), rows[k+1:]...)
		v := VerifyAuditChain(AuditGenesisHash, cut)
		return !v.OK() &&
			v.EventsChecked == uint64(k) &&
			v.FirstBrokenSeq != nil && *v.FirstBrokenSeq == wantSeq
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// ADR-0004's documented residual gap, pinned as spec: truncating the
// NEWEST rows (and nothing after them) leaves no broken link — a prefix
// of an honest chain always verifies. Non-repudiation of tail deletion is
// the JSONL export's job, not the chain's.
func TestChainPropertyTailTruncationIsUndetectable(t *testing.T) {
	f := func(gs []genEvent, idx uint8) bool {
		rows := chainRows(toEvents(gs))
		k := 0
		if len(rows) > 0 {
			k = int(idx) % (len(rows) + 1)
		}
		v := VerifyAuditChain(AuditGenesisHash, rows[:k])
		return v.OK() && v.EventsChecked == uint64(k)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// Inserting a row whose hash was not honestly chained from its
// predecessor breaks verification at the insertion point. (The forged
// hash here is random hex; an attacker who instead recomputes every later
// hash is the ADR's accepted tamper-EVIDENCE limit, not a detectable
// case.)
func TestChainPropertyInsertionAlwaysBreaks(t *testing.T) {
	const hexDigits = "0123456789abcdef"
	f := func(gs []genEvent, forged genEvent, idx uint8, seed int64) bool {
		events := toEvents(gs)
		rows := chainRows(events)
		k := 0
		if len(rows) > 0 {
			k = int(idx) % (len(rows) + 1)
		}
		rng := rand.New(rand.NewSource(seed))
		hash := make([]byte, 64)
		for i := range hash {
			hash[i] = hexDigits[rng.Intn(16)]
		}
		forgedRow := ChainedAuditRow{Seq: uint64(k) + 1, Event: core.AuditEvent(forged), ChainHash: string(hash)}
		cut := make([]ChainedAuditRow, 0, len(rows)+1)
		cut = append(cut, rows[:k]...)
		cut = append(cut, forgedRow)
		cut = append(cut, rows[k:]...)
		v := VerifyAuditChain(AuditGenesisHash, cut)
		return !v.OK() &&
			v.EventsChecked == uint64(k) &&
			v.FirstBrokenSeq != nil && *v.FirstBrokenSeq == forgedRow.Seq
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// Mid-trail windows: a suffix window verifies against the hash of the row
// just before it (Store.AuditChain's contract for paginated verification),
// and the same window against the WRONG head breaks at its first row.
func TestChainPropertyWindowVerification(t *testing.T) {
	f := func(gs []genEvent, idx uint8) bool {
		rows := chainRows(toEvents(gs))
		if len(rows) == 0 {
			return true
		}
		m := int(idx) % (len(rows) + 1)
		head := AuditGenesisHash
		if m > 0 {
			head = rows[m-1].ChainHash
		}
		v := VerifyAuditChain(head, rows[m:])
		if !v.OK() || v.EventsChecked != uint64(len(rows)-m) {
			return false
		}
		// Wrong head on a non-empty, non-genesis window breaks at row 0 of
		// the window (with probability 1-2^-256: a collision would pass).
		if m > 0 && m < len(rows) {
			wrong := VerifyAuditChain(AuditGenesisHash, rows[m:])
			return !wrong.OK() && wrong.EventsChecked == 0 &&
				wrong.FirstBrokenSeq != nil && *wrong.FirstBrokenSeq == rows[m].Seq
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// AuditChainHash's output contract: 64 lowercase hex chars, deterministic
// in (prevHash, event), and prevHash-sensitive.
func TestChainPropertyHashShape(t *testing.T) {
	f := func(e genEvent) bool {
		ev := core.AuditEvent(e)
		h := AuditChainHash(AuditGenesisHash, &ev)
		if len(h) != 64 || h != AuditChainHash(AuditGenesisHash, &ev) {
			return false
		}
		for i := 0; i < len(h); i++ {
			c := h[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false // lowercase hex only
			}
		}
		// Chaining the row's own hash as the next prev must change the
		// output (a fixed point would let a row attest itself).
		return AuditChainHash(h, &ev) != h
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}
