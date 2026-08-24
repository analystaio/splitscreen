package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the wire protocol version this build speaks.
//
// Gateway and runners deploy independently, so a rolling upgrade always has
// mismatched versions in flight. Bump Major for any change that removes a
// frame, removes a field, or changes the meaning of an existing field. Bump
// Minor for additive changes (new frame types, new optional fields).
const Version = "1.1"

// SemVer is a major.minor pair. The protocol deliberately has no patch
// component: a change either affects the wire or it does not.
type SemVer struct {
	Major int
	Minor int
}

func (v SemVer) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
}

// ParseVersion parses a "major.minor" string.
func ParseVersion(s string) (SemVer, error) {
	major, minor, ok := strings.Cut(s, ".")
	if !ok {
		return SemVer{}, fmt.Errorf("protocol: malformed version %q, want major.minor", s)
	}
	maj, err := strconv.Atoi(major)
	if err != nil || maj < 0 {
		return SemVer{}, fmt.Errorf("protocol: malformed major version in %q", s)
	}
	min, err := strconv.Atoi(minor)
	if err != nil || min < 0 {
		return SemVer{}, fmt.Errorf("protocol: malformed minor version in %q", s)
	}
	return SemVer{Major: maj, Minor: min}, nil
}

// Ours is the parsed form of Version.
func Ours() SemVer {
	v, err := ParseVersion(Version)
	if err != nil {
		panic("protocol: bad build-time Version constant: " + err.Error())
	}
	return v
}

// Compatibility is the outcome of negotiating with a peer.
type Compatibility int

const (
	// Incompatible means the connection must be refused.
	Incompatible Compatibility = iota
	// CompatibleWithWarning means the peer speaks an older or newer minor of
	// the same major. The connection proceeds; the discrepancy is logged and
	// surfaced in status output so a half-finished rollout is visible.
	CompatibleWithWarning
	// Compatible means an exact match.
	Compatible
)

func (c Compatibility) OK() bool { return c != Incompatible }

// Negotiate compares a peer's version against ours.
//
// Policy: same major is required, minor skew is tolerated in both directions.
// A newer peer minor is tolerated because additive changes are, by definition,
// ignorable by an older reader.
func Negotiate(peer SemVer) Compatibility {
	ours := Ours()
	switch {
	case peer.Major != ours.Major:
		return Incompatible
	case peer.Minor != ours.Minor:
		return CompatibleWithWarning
	default:
		return Compatible
	}
}

// NegotiateString is Negotiate over the raw string from a hello frame.
func NegotiateString(peer string) (SemVer, Compatibility, error) {
	v, err := ParseVersion(peer)
	if err != nil {
		return SemVer{}, Incompatible, err
	}
	return v, Negotiate(v), nil
}
