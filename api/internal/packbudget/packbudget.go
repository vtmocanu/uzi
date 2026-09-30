// Package packbudget is the reconstructed-size pre-scan of a git packfile, shared by the two
// places the api decodes a pack it did not build into go-git's unbounded in-memory storer:
// pushbroker (a worker's checkpoint pack) and agentsource (the pack an untrusted skills or
// agent-source remote sends back). Scan walks the pack and refuses it, before any object is
// resolved, when it would inflate past a Limits bound. A compressed-size cap alone does not
// bound the heap: a zlib bomb or a delta declaring a huge target is small on the wire.
package packbudget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

var (
	// ErrTooLarge means the pack exceeds a Limits bound along any axis (object count,
	// per-object inflated/declared size, cumulative reconstructed bytes, cumulative
	// inflation work).
	ErrTooLarge = errors.New("packbudget: pack exceeds inflation budget")
	// ErrInvalid means the pack header or an object header could not be parsed: a genuinely
	// malformed pack, distinct from an oversize one.
	ErrInvalid = errors.New("packbudget: pack is malformed")
)

// Bound names the Limits axis that tripped, so an admin can tell which limit to act on.
type Bound string

// The four Limits axes.
const (
	BoundObjects       Bound = "object count"
	BoundObjectBytes   Bound = "per-object size"
	BoundTotalBytes    Bound = "total reconstructed size"
	BoundInflationWork Bound = "inflation work"
)

// BudgetError is the error Scan returns when a Limits bound is exceeded. It matches ErrTooLarge
// under errors.Is, and its text names the bound and the limit; it carries only sizes, never a
// URL or credential.
type BudgetError struct {
	Bound Bound
	Limit int64
}

func (e *BudgetError) Error() string {
	unit := "bytes"
	if e.Bound == BoundObjects {
		unit = "objects"
	}
	return fmt.Sprintf("%s: %s limit of %d %s exceeded", ErrTooLarge.Error(), e.Bound, e.Limit, unit)
}

// Is makes a BudgetError match the ErrTooLarge sentinel.
func (e *BudgetError) Is(target error) bool { return target == ErrTooLarge }

// Limits are the four bounds Scan enforces.
type Limits struct {
	// ObjectBytes caps ONE object's declared size (the target size for a delta).
	ObjectBytes int64
	// TotalBytes caps the cumulative RECONSTRUCTED size (the heap the apply allocates).
	TotalBytes int64
	// InflationWorkBytes caps the cumulative bytes Scan zlib-inflates across every object,
	// delta instruction streams included (the CPU defense).
	InflationWorkBytes int64
	// Objects caps the object count in the pack header.
	Objects uint32
}

// Scan walks pack and returns ErrTooLarge the instant any Limits bound is exceeded, before any
// object is resolved into a storer. See the per-axis rationale in pushbroker's budget comment:
// a delta's header Length is only its instruction stream, so the reconstructed target size
// (the second varint of the delta header) is read and capped too, and the cumulative inflation
// work is capped separately because a tiny-target delta behind a huge instruction stream costs
// CPU without costing heap. It honours ctx at the top of every object. Because it validates the
// same immutable []byte the caller then decodes, there is no TOCTOU. An empty pack is accepted.
func Scan(ctx context.Context, pack []byte, lim Limits) error {
	if len(pack) == 0 {
		return nil
	}
	scanner := packfile.NewScanner(bytes.NewReader(pack))
	_, objects, err := scanner.Header()
	if err != nil {
		return ErrInvalid
	}
	if objects > lim.Objects {
		return &BudgetError{BoundObjects, int64(lim.Objects)}
	}
	var total, inflationWork int64
	for i := uint32(0); i < objects; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := scanner.NextObjectHeader()
		if err != nil {
			if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
				return &BudgetError{BoundObjectBytes, lim.ObjectBytes}
			}
			return ErrInvalid
		}
		if h.Length < 0 || h.Length > lim.ObjectBytes {
			return &BudgetError{BoundObjectBytes, lim.ObjectBytes}
		}
		inflationWork += h.Length
		if inflationWork > lim.InflationWorkBytes {
			return &BudgetError{BoundInflationWork, lim.InflationWorkBytes}
		}

		var contributed int64
		switch h.Type {
		case plumbing.OFSDeltaObject, plumbing.REFDeltaObject:
			var buf bytes.Buffer
			if _, _, err := scanner.NextObject(&buf); err != nil {
				if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
					return &BudgetError{BoundObjectBytes, lim.ObjectBytes}
				}
				return ErrInvalid
			}
			b := buf.Bytes()
			if _, b, err = ReadDeltaVarint(b); err != nil { // base size (discard)
				return ErrInvalid
			}
			var targetSz int64
			if targetSz, _, err = ReadDeltaVarint(b); err != nil { // reconstructed size
				return ErrInvalid
			}
			if targetSz > lim.ObjectBytes {
				return &BudgetError{BoundObjectBytes, lim.ObjectBytes}
			}
			contributed = targetSz
		default:
			if _, _, err := scanner.NextObject(io.Discard); err != nil {
				if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
					return &BudgetError{BoundObjectBytes, lim.ObjectBytes}
				}
				return ErrInvalid
			}
			contributed = h.Length
		}

		total += contributed
		if total > lim.TotalBytes {
			return &BudgetError{BoundTotalBytes, lim.TotalBytes}
		}
	}
	return nil
}

// ReadDeltaVarint decodes one unsigned little-endian base-128 varint from the head of b using
// git's DELTA-header size encoding (low 7 bits per byte, continue while 0x80 is set) and returns
// the value, the bytes after it, and an error on a truncated stream or a shift that would
// overflow int64. It is DELIBERATELY not the packfile object-header length encoding, which packs
// the type into the first byte and shifts differently.
func ReadDeltaVarint(b []byte) (val int64, rest []byte, err error) {
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if shift >= 63 {
			return 0, nil, errors.New("packbudget: delta varint overflow")
		}
		val |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return val, b[i+1:], nil
		}
	}
	return 0, nil, errors.New("packbudget: delta varint truncated")
}
