package mcp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tamper-evident audit log (improvement: 감사로그 무결성). Every audit entry
// carries a monotonic seq and a hash that chains to the previous entry:
//
//	hash_n = sha256(prev_hash || canonical_json(entry_without_chain_fields))
//
// A single deleted, reordered, or edited line breaks the chain and is pinpointed
// by VerifyAuditChain. All audit writers funnel through appendAudit so the chain
// stays consistent under concurrency (auditMu serializes appends).

type auditTipRec struct {
	Seq  int64
	Hash string
}

// appendAudit chains and writes one audit entry to today's audit file. It never
// returns an error (audit is best-effort side-channel) but preserves integrity
// fields when it does write.
func (s *Server) appendAudit(entry map[string]any) {
	dir := filepath.Join(s.opDir(), "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "audit-"+time.Now().Format("20060102")+".jsonl")

	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditTips == nil {
		s.auditTips = map[string]auditTipRec{}
	}
	tip, ok := s.auditTips[path]
	if !ok {
		tip = lastAuditTip(path) // re-seed from disk after a restart
	}

	// chain fields are excluded from the hashed payload, then attached.
	delete(entry, "seq")
	delete(entry, "prev")
	delete(entry, "hash")
	payload := canonicalJSON(entry)
	h := sha256.Sum256([]byte(tip.Hash + "\n" + payload))
	next := auditTipRec{Seq: tip.Seq + 1, Hash: hex.EncodeToString(h[:])}

	entry["seq"] = next.Seq
	entry["prev"] = tip.Hash
	entry["hash"] = next.Hash
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return
	}
	s.auditTips[path] = next
}

// canonicalJSON serializes a map with sorted keys so the hash is stable
// regardless of Go's map iteration order.
func canonicalJSON(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// lastAuditTip reads the final well-formed entry from an audit file to resume
// the chain across restarts. Returns the zero tip for a missing/empty file.
func lastAuditTip(path string) auditTipRec {
	f, err := os.Open(path)
	if err != nil {
		return auditTipRec{}
	}
	defer f.Close()
	var last auditTipRec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		}
		if json.Unmarshal(line, &e) == nil && e.Hash != "" {
			last = auditTipRec{Seq: e.Seq, Hash: e.Hash}
		}
	}
	return last
}

// VerifyAuditChain recomputes the hash chain for one day's audit file and
// reports the first break (if any). day is "YYYYMMDD"; empty = today.
func (s *Server) VerifyAuditChain(day string) map[string]any {
	if day == "" {
		day = time.Now().Format("20060102")
	}
	if len(day) != 8 || strings.ContainsAny(day, "/\\.") {
		return map[string]any{"error": "day must be YYYYMMDD"}
	}
	path := filepath.Join(s.opDir(), "audit", "audit-"+day+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"day": day, "entries": 0, "valid": true, "note": "해당 일자 감사 로그가 없습니다."}
		}
		return map[string]any{"error": err.Error()}
	}
	defer f.Close()

	prev := ""
	var expectedSeq int64 = 1
	entries := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			return chainBreak(day, entries, lineNo, "invalid JSON")
		}
		gotHash, _ := e["hash"].(string)
		gotPrev, _ := e["prev"].(string)
		var gotSeq int64
		if f, ok := e["seq"].(float64); ok {
			gotSeq = int64(f)
		}
		if gotHash == "" {
			return chainBreak(day, entries, lineNo, "entry has no hash (pre-chain or tampered)")
		}
		if gotPrev != prev {
			return chainBreak(day, entries, lineNo, "prev hash mismatch (deletion/reorder)")
		}
		if gotSeq != expectedSeq {
			return chainBreak(day, entries, lineNo, fmt.Sprintf("seq gap: got %d want %d", gotSeq, expectedSeq))
		}
		clone := map[string]any{}
		for k, v := range e {
			if k == "seq" || k == "prev" || k == "hash" {
				continue
			}
			clone[k] = v
		}
		want := sha256.Sum256([]byte(prev + "\n" + canonicalJSON(clone)))
		if hex.EncodeToString(want[:]) != gotHash {
			return chainBreak(day, entries, lineNo, "content hash mismatch (entry edited)")
		}
		prev = gotHash
		expectedSeq++
		entries++
	}
	return map[string]any{"day": day, "entries": entries, "valid": true,
		"note": "감사 로그 해시 체인이 온전합니다.", "tip_hash": prev}
}

func chainBreak(day string, verified, line int, reason string) map[string]any {
	return map[string]any{
		"day": day, "valid": false, "verified_entries": verified,
		"broken_at_line": line, "reason": reason,
		"note": "감사 로그가 변조/삭제/재배열되었을 수 있습니다. 이 라인 이후는 신뢰할 수 없습니다.",
	}
}
