// Shared helpers: hashing, encodings, deterministic ids and timestamps.
// Mirrors src/util.ts exactly (same constants, same timestamp behavior).
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const ZeroOID = "0000000000000000000000000000000000000000"

const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"
const ViewerType = "User"

// wire.ts SCENARIO_EPOCH_SEC (2025-01-01T00:00:00Z).
const EpochSec = 1735689600

func sha1Hex(data ...[]byte) string {
	h := sha1.New()
	for _, d := range data {
		h.Write(d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha1String(s string) string {
	return sha1Hex([]byte(s))
}

// isoOfSeq mirrors util.ts: ISO-8601 for EPOCH_SEC + seq seconds, cached.
// Guarded by its own mutex: read-only requests populate it while holding
// only a read lock on the store.
var isoCache = map[int]string{}
var isoCacheMu sync.Mutex

func isoOfSeq(seq int) string {
	isoCacheMu.Lock()
	defer isoCacheMu.Unlock()
	if v, ok := isoCache[seq]; ok {
		return v
	}
	s := time.Unix(int64(EpochSec+seq), 0).UTC().Format("2006-01-02T15:04:05.000Z")
	// Go's Format with .000Z prints millis; TS toISOString prints .000Z too.
	isoCache[seq] = s
	return s
}

func b64encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

var wsRe = regexp.MustCompile(`[\r\n\t ]+`)

func b64decode(text string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(wsRe.ReplaceAllString(text, ""))
}

func b64urlEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func b64urlDecode(text string) (string, bool) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(text) {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// b64urlDecodeStr is b64urlDecode with a clearer name for the GraphQL use.
func b64urlDecodeStr(text string) (string, bool) { return b64urlDecode(text) }

// timeUnix returns the UTC time for an epoch second.
func timeUnix(epoch int64) time.Time {
	return time.Unix(epoch, 0).UTC()
}

// atoiSafe parses a decimal integer string, tolerating a leading '-'.
func atoiSafe(s string) (int, error) {
	n := 0
	neg := false
	i := 0
	if len(s) > 0 && s[0] == '-' {
		neg = true
		i = 1
	}
	if i >= len(s) {
		return 0, fmt.Errorf("not a number")
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// TickClock mirrors util.ts: EPOCH_SEC plus a counter, one second a tick.
type TickClock struct {
	tick int
}

func (c *TickClock) NextSec() int {
	c.tick++
	return EpochSec + c.tick
}

func (c *TickClock) NextISO() string {
	return time.Unix(int64(c.NextSec()), 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isOID(s string) bool {
	return oidRe.MatchString(s)
}

func identityLine(name, email string, epochSec int) string {
	return fmt.Sprintf("%s <%s> %d +0000", name, email, epochSec)
}

func short(value string, max int) string {
	if len(value) > max {
		return value[:max] + "…"
	}
	return value
}

// pushCommandRe matches one receive-pack command line:
// "<old> <new> <ref>".
var pushCommandRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$`)

// jsonString renders a string as JSON.stringify would (with quotes).
func jsonString(s string) string {
	return string(marshalJSON(s))
}

func stringsContains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
