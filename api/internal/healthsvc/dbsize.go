package healthsvc

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// dbSizeCacheTTL is how long one replica reuses a db.size result, success or failure, so a
// frequently polled health page does not scan pg_class on every evaluation.
// maxLargestRelations caps the largest-relation evidence rows.
const maxLargestRelations = 3

const dbSizeCacheTTL = 60 * time.Second

// checkDBSize compares pg_database_size with the configured database-size budget
// (DB_STORAGE_CAPACITY_BYTES). The budget is operator-declared, not a measured volume
// size. The result is cached per replica for dbSizeCacheTTL on the injected clock.
func (s *Service) checkDBSize(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	s.dbSizeMu.Lock()
	defer s.dbSizeMu.Unlock()
	if s.dbSizeInitialized && now.Sub(s.dbSizeRefreshedAt) < dbSizeCacheTTL {
		return copyPricingCheck(s.dbSizeResult)
	}
	c := s.evalDBSize(ctx)
	s.dbSizeResult = c
	s.dbSizeRefreshedAt = now
	s.dbSizeInitialized = true
	return copyPricingCheck(c)
}

func (s *Service) evalDBSize(ctx context.Context) apitypes.HealthCheckDTO {
	c := s.base("db.size")
	capacity := s.cfg.DBStorageCapacityBytes
	var (
		size store.DatabaseSize
		err  error
	)
	if s.probeDBSize == nil {
		err = errDBSizeProbeMissing
	} else {
		size, err = s.probeDBSize(ctx)
	}

	if capacity <= 0 {
		c.Severity = sevNA
		c.Summary = "No database storage capacity is configured (DB_STORAGE_CAPACITY_BYTES)."
		if err == nil {
			c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: "Size", Value: humanBytes(size.SizeBytes)})
		}
		return c
	}
	if err != nil {
		if s.probeDBSize == nil {
			c.Severity = sevUnknown
			c.Summary = "The database size probe is not configured."
			return c
		}
		return degradeUnknown(c, "db.size", err)
	}

	// Exact integer bands. size*100 >= capacity*percent is evaluated in big.Int so it cannot
	// overflow for any int64 pair (capacity is at most 2^60, so capacity*100 alone could
	// otherwise wrap near the bound).
	switch {
	case atLeastPercent(size.SizeBytes, capacity, dbSizeDangerPercent):
		c.Severity = sevDanger
	case atLeastPercent(size.SizeBytes, capacity, dbSizeWarnPercent):
		c.Severity = sevWarn
	default:
		c.Severity = sevOK
	}
	tenths := percentTenths(size.SizeBytes, capacity)
	c.Summary = fmt.Sprintf("Database is using %d%% of its configured storage capacity.", tenths/10)
	c.Evidence = append(c.Evidence,
		apitypes.HealthEvidenceDTO{Label: "Size", Value: humanBytes(size.SizeBytes)},
		apitypes.HealthEvidenceDTO{Label: "Capacity", Value: humanBytes(capacity)},
		apitypes.HealthEvidenceDTO{Label: "Used", Value: fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)},
	)
	for i, r := range size.Largest {
		if i == maxLargestRelations {
			break
		}
		c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: "Largest relation", Value: safe(r.Name) + ": " + humanBytes(r.SizeBytes)})
	}
	if c.Severity != sevOK {
		c.Action = strPtr("Grow the database volume and update DB_STORAGE_CAPACITY_BYTES to match, or reduce the data stored. This compares database size to a configured budget, not volume usage; see the admin health docs.")
	}
	return c
}

// atLeastPercent reports size*100 >= capacity*percent without overflow.
func atLeastPercent(size, capacity int64, percent int64) bool {
	l := new(big.Int).Mul(big.NewInt(size), big.NewInt(100))
	r := new(big.Int).Mul(big.NewInt(capacity), big.NewInt(percent))
	return l.Cmp(r) >= 0
}

// percentTenths returns floor(size*1000/capacity), the used percentage in tenths, so the
// displayed figure never rounds up across a band boundary. capacity must be positive.
func percentTenths(size, capacity int64) int64 {
	if size < 0 {
		size = 0
	}
	n := new(big.Int).Mul(big.NewInt(size), big.NewInt(1000))
	n.Quo(n, big.NewInt(capacity))
	if !n.IsInt64() {
		return 1<<62 - 1
	}
	return n.Int64()
}

// humanBytes renders a byte count with binary units, e.g. "1.5 GiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type dbSizeProbeError string

func (e dbSizeProbeError) Error() string { return string(e) }

const errDBSizeProbeMissing = dbSizeProbeError("database size probe not configured")

// livePoolSizeProbe is the production db.size probe, bounded by a 5s timeout.
func (s *Service) livePoolSizeProbe(ctx context.Context) (store.DatabaseSize, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return store.DatabaseSizeStatus(ctx, s.cfg.Pool)
}
