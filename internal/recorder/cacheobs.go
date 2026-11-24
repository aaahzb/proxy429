package recorder

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Observed upstream cache-lifetime measurement (status page 「缓存命中」 popup's observed table; display-only, in-memory).
//
// Two adjacent finished streams of the same session + same upstream (obsKey = convID|upstreamKey) yield one observation:
//   - latter hit rate ≥95% → alive observation: the cache lived at least as long as the interval between the two streams' starts; take max as the observed lower bound;
//   - former had hits + latter hit rate <50% → dead observation: the cache did not survive that interval; take min as the observed upper bound
//     (strict zero not required: the upstream caches common prefixes like system prompts, so scattered hits can persist after the session cache dies);
//   - the middle band (50%~95%) is not observed: can't tell cache expiry from input drift;
//   - input volume <1024 tokens is not observed: small requests have noisy hit rates;
//   - interval ≤0 (clock jumps etc.) is not observed;
//   - on contradiction (the upstream cache lifetime changed mid-way, or the cache was evicted early), the newer observation wins
//     and the refuted side is discarded and re-measured — the display can never invert (e.g. "≥18min, <16min").
// It measures a lower bound, not the true TTL: a conversation the user stops following up on never produces a death signal.
// cacheObsMap shares finishedMu with finished and is cleared together by clearStats (restarting clears it naturally).

type CacheObsKind int

const (
	ObsNone  CacheObsKind = iota // Does not constitute an observation
	ObsAlive                     // Alive observation: the cache lived at least as long as the interval
	ObsDead                      // Dead observation: the cache did not survive the interval
)

const cacheAliveHitRate = 0.95 // A latter-stream hit rate ≥95% counts as "still within the cache"

const cacheDeadHitRate = 0.50 // Former had hits + latter hit rate <50% counts as cache lost (residual hits on common prefixes like system prompts don't count as alive)

const cacheObsMinTokens = 1024 // Input volume (input+cacheRead+cacheCreation) below this is not observed

const CacheObsMaxKeyLen = 512 // Truncate abnormally long obsKeys (malicious session_id) to prevent memory bloat

type CacheObsEntry struct {
	AliveMax time.Duration // Max alive-observation interval (observed lower bound: the cache survived at least this long)
	DeadMin  time.Duration // Min dead-observation interval (observed upper bound: the cache did not survive this long); 0 = no dead observation yet
	Samples  int           // Observation count (alive + dead)

	// aliveAt/deadAt are the moments each bound's value last changed: reset when the bound's value changes (including contradiction invalidation),
	// and the invalidated-to-remeasure side is zeroed — when a bound doesn't exist, its formation time doesn't either.
	AliveAt time.Time
	DeadAt  time.Time
}

var CacheObsMap = map[string]*CacheObsEntry{} // Key = upstreamKey (url|model); guarded by finishedMu

// classifyCacheObservation decides what observation a finished stream forms relative to the previous one of the same session + same upstream.
// Death doesn't require a strict zero (the upstream caches common prefixes like system prompts, so scattered cache_read
// can persist after the session cache expires); a hit rate <50% counts as "cache lost". When it returns obsNone, the second return value is meaningless.
func ClassifyCacheObservation(prevCacheRead, curCacheRead, curInput, curCacheCreation int64, interval time.Duration) (CacheObsKind, time.Duration) {
	if interval <= 0 || curInput+curCacheRead+curCacheCreation < cacheObsMinTokens {
		return ObsNone, 0
	}
	hitRate := float64(curCacheRead) / float64(curInput+curCacheRead+curCacheCreation)
	// Check death first: the former having hits proves the cache was written; the latter's hit rate <50% proves the session cache didn't survive until now
	// (the two branches are mutually exclusive: a hit rate can't be both <50% and ≥95%; the order is only for readability).
	if prevCacheRead > 0 && hitRate < cacheDeadHitRate {
		return ObsDead, interval
	}
	if hitRate >= cacheAliveHitRate {
		return ObsAlive, interval
	}
	return ObsNone, 0
}

// recordCacheObsLocked accumulates one observation into the upstream's entry: alive takes max (lower bound only rises), dead takes min (upper bound only falls).
// On contradiction the newer observation wins and the refuted side is discarded and re-measured: if the upstream cache lifetime grew mid-way (a new alive observation crossed the old upper bound),
// the upper bound is discarded; if it shrank or the cache was evicted early (a new dead observation fell below the old lower bound), the lower bound is discarded.
// Each bound's formation time is accounted separately: it moves only when the bound's value changes (including being invalidated to zero), not when a supporting observation is merely added.
// Invariant: when both sides are non-zero, aliveMax < deadMin (the display never inverts). Caller must hold finishedMu.
func RecordCacheObsLocked(key string, kind CacheObsKind, interval time.Duration) {
	e := CacheObsMap[key]
	if e == nil {
		e = &CacheObsEntry{}
		CacheObsMap[key] = e
	}
	now := time.Now()
	if kind == ObsAlive {
		if interval > e.AliveMax {
			e.AliveMax = interval
			e.AliveAt = now // Restart the clock only when the lower bound's value changes
		}
		if e.DeadMin > 0 && e.DeadMin <= e.AliveMax {
			e.DeadMin = 0 // Old upper bound refuted by new alive evidence (TTL grew / old upper bound was noise) — discard and re-measure
			e.DeadAt = time.Time{}
			e.Samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	if kind == ObsDead {
		if e.DeadMin == 0 || interval < e.DeadMin {
			e.DeadMin = interval
			e.DeadAt = now // Restart the clock only when the upper bound's value changes
		}
		if e.AliveMax >= e.DeadMin {
			e.AliveMax = 0 // Old lower bound refuted by new dead evidence (TTL shrank / early eviction) — discard and re-measure
			e.AliveAt = time.Time{}
			e.Samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	e.Samples++
}

// obsKeyOf returns the observed cache-lifetime pairing key (convID|upstreamKey); an empty string means "not observed" when either side is missing (count_tokens probes,
// bare API calls without metadata, missing upstream grouping key).
func ObsKeyOf(f *Flight) string {
	if f.ConvID == "" || f.UpstreamKey == "" {
		return ""
	}
	k := f.ConvID + "|" + f.UpstreamKey
	if len(k) > CacheObsMaxKeyLen {
		k = k[:CacheObsMaxKeyLen]
	}
	return k
}

// cacheObsRow is a JSON snapshot entry of observed cache lifetimes (for the cache-hit popup's 「实测缓存时间」 table).
type CacheObsRow struct {
	URL     string `json:"url"`     // Upstream base URL (scheme stripped to shorten the display)
	Model   string `json:"model"`   // Model actually sent
	Alive   string `json:"alive"`   // Lower-bound reading (e.g. "23min"); "" when there is no alive observation
	Dead    string `json:"dead"`    // Upper-bound reading; "" when there is no dead observation
	Samples int    `json:"samples"` // Observation count (alive + dead)

	// AliveAge/DeadAge are how long each bound's value has stood (the clock restarts when the bound's value changes), counting up as m:ss;
	// "" when the corresponding bound has no observation (Alive/Dead is "").
	AliveAge string `json:"aliveAge"`
	DeadAge  string `json:"deadAge"`
}

// snapshotCacheObs returns observed cache-lifetime snapshots for all upstreams, sorted by URL+model (stable display).
func SnapshotCacheObs() []CacheObsRow {
	FinishedMu.Lock()
	defer FinishedMu.Unlock()
	if len(CacheObsMap) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]CacheObsRow, 0, len(CacheObsMap))
	for key, e := range CacheObsMap {
		url, model, _ := strings.Cut(key, "|")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		row := CacheObsRow{URL: url, Model: model, Samples: e.Samples}
		if e.AliveMax > 0 {
			row.Alive = fmtObsDur(e.AliveMax)
			if !e.AliveAt.IsZero() {
				row.AliveAge = FmtCacheAge(now.Sub(e.AliveAt))
			}
		}
		if e.DeadMin > 0 {
			row.Dead = fmtObsDur(e.DeadMin)
			if !e.DeadAt.IsZero() {
				row.DeadAge = FmtCacheAge(now.Sub(e.DeadAt))
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// FmtCacheAge formats a cache age (time since the cache was created) as m:ss (h:mm:ss when ≥1h).
func FmtCacheAge(d time.Duration) string {
	s := int(d.Seconds())
	if h := s / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// fmtObsDur formats an observed interval as mm:ss ("00:30" / "23:30", minutes zero-padded to two digits; ≥1h uses the same h:mm:ss form as cache age, e.g. "4:44:13").
func fmtObsDur(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return FmtCacheAge(d)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}
