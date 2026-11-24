package main

import (
	"bytes"
	"encoding/json"
	"hash/fnv"

	"proxy429/internal/jsonutil"
)

// lineageHashCap bounds one flight's stored message hashes (a memory guard for pathological histories);
// beyond the cap the chain simply stops lengthening (prefix matching up to the cap still works).
const lineageHashCap = 4096

// lineageTailCap bounds each flight's stored copy of its chain's last element (normalized raw bytes), kept so a
// candidate whose divergence point is exactly its own last element can still earn a tail byte-prefix score.
const lineageTailCap = 32 << 10

// stripCacheControl removes "cache_control":{...} members from a raw JSON fragment. Claude Code migrates ephemeral
// cache breakpoints between turns, so the same logical element replays with the marker added, moved, or removed;
// lineage hashing must ignore markers or every such turn breaks the chain. Works on raw bytes without parsing:
// match the key, skip whitespace and the colon, balanced-scan the object (string-aware), then swallow one adjacent
// comma (the preceding one when present, else the following one). JSON string contents are safe: inside a string the
// key's quotes arrive escaped (\"), so the literal key never matches there. Content that legitimately holds such a
// member (e.g. a tool input_schema) normalizes identically on both sides of a comparison, so equality is unaffected.
func stripCacheControl(b []byte) []byte {
	key := []byte(`"cache_control"`)
	for i := 0; i < len(b); {
		j := bytes.Index(b[i:], key)
		if j < 0 {
			break
		}
		start := i + j
		m := start + len(key)
		for m < len(b) && (b[m] == ' ' || b[m] == '\t' || b[m] == '\r' || b[m] == '\n') {
			m++
		}
		if m >= len(b) || b[m] != ':' {
			i = start + len(key) // Not a member shaped like a marker; skip past the key.
			continue
		}
		m++
		for m < len(b) && (b[m] == ' ' || b[m] == '\t' || b[m] == '\r' || b[m] == '\n') {
			m++
		}
		if m >= len(b) || b[m] != '{' {
			i = start + len(key)
			continue
		}
		// Balanced scan for the object's end (string-aware; marker objects are small and flat in practice).
		depth, inStr, esc, end := 0, false, false, -1
		for p := m; p < len(b); p++ {
			c := b[p]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = p + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break // Unbalanced (truncated fragment): leave the remainder untouched.
		}
		// Swallow one adjacent comma so a removed member leaves no dangling separator.
		if start > 0 && b[start-1] == ',' {
			start--
		} else if end < len(b) && b[end] == ',' {
			end++
		}
		b = append(b[:start], b[end:]...)
		i = start // Content shifted left; rescan from the removal point for further markers.
	}
	return b
}

// hashLineage normalizes (cache_control markers stripped) and hashes each element of the request's message array
// (Anthropic "messages" or Responses "input") into the flight's lineage chain (fnv64a per element). Apart from
// breakpoint migration and interior tail growth (see findLineageParent), clients replay history byte-identically,
// so normalized raw bytes are a stable element identity; the body is never fully decoded. Returns the normalized
// elements and their hashes (nil for non-JSON bodies or bodies without a message array).
func hashLineage(body []byte) ([][]byte, []uint64) {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return nil, nil
	}
	for _, s := range spans {
		if s.Name != "messages" && s.Name != "input" {
			continue
		}
		var raw []json.RawMessage
		if json.Unmarshal(body[s.ValStart:s.ValEnd], &raw) != nil || len(raw) == 0 {
			return nil, nil
		}
		n := min(len(raw), lineageHashCap)
		elems := make([][]byte, 0, n)
		hashes := make([]uint64, 0, n)
		for i, e := range raw {
			if i >= lineageHashCap {
				break
			}
			norm := stripCacheControl(e)
			h := fnv.New64a()
			_, _ = h.Write(norm)
			elems = append(elems, norm)
			hashes = append(hashes, h.Sum64())
		}
		return elems, hashes
	}
	return nil, nil
}

// commonPrefixLen returns the length of the leading byte run a and b share.
func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// findLineageParent resolves a request's parent flight among the earlier same-session flights (active registry +
// finished ring) by a two-part chain score: (1) the count of leading byte-identical elements (must be ≥1 — sharing
// only the JSON shape or the session id links nothing, which is what keeps sub-agent branches and /clear restarts
// parentless); (2) when the first difference is exactly the candidate's own last element — the client grew that
// message's content array interiorly between turns — the common byte-prefix length of the two tails. Longer full
// prefix wins, then longer tail, then a fully contained candidate beats a merely prefix-sharing one (a rewind links
// to the forked-from state, not the abandoned branch tip), and ties break to the newest id. 0 = no parent.
func findLineageParent(convID string, id uint64, elems [][]byte, hashes []uint64) uint64 {
	var best uint64
	bestFull, bestTail := 0, 0
	bestContained := false
	consider := func(cid string, fid uint64, cand []uint64, candTail []byte) {
		if cid != convID || fid >= id || len(cand) == 0 {
			return
		}
		full := 0
		for full < len(cand) && full < len(hashes) && cand[full] == hashes[full] {
			full++
		}
		if full < 1 {
			return
		}
		contained := full == len(cand)
		tail := 0
		if !contained && full == len(cand)-1 && full < len(elems) && len(candTail) > 0 {
			tail = commonPrefixLen(candTail, elems[full])
		}
		if full > bestFull ||
			(full == bestFull && tail > bestTail) ||
			(full == bestFull && tail == bestTail && contained && !bestContained) ||
			(full == bestFull && tail == bestTail && contained == bestContained && fid > best) {
			best, bestFull, bestTail, bestContained = fid, full, tail, contained
		}
	}
	for _, f := range flights.snapshot() {
		consider(f.convID, f.id, f.msgHashes, f.msgTail)
	}
	finishedMu.Lock()
	for i := range finished {
		consider(finished[i].convID, finished[i].id, finished[i].msgHashes, finished[i].msgTail)
	}
	finishedMu.Unlock()
	return best
}

// computeLineage fills a flight's message-hash chain, tail copy, and parent link at request receipt (the handler
// calls it right after convID is settled, on the body the lineage should reflect). Requests without a session
// identifier or a message array stay parentless; the web then shows a bare #N.
func computeLineage(f *flight, body []byte) {
	elems, hashes := hashLineage(body)
	f.msgHashes = hashes
	if len(elems) > 0 {
		tail := elems[len(elems)-1]
		if len(tail) > lineageTailCap {
			tail = tail[:lineageTailCap]
		}
		f.msgTail = bytes.Clone(tail) // Clone: the flight outlives the parsed body; don't pin the whole element backing.
	}
	if f.convID == "" || len(hashes) == 0 {
		return
	}
	f.lastFlight = findLineageParent(f.convID, f.id, elems, hashes)
}
