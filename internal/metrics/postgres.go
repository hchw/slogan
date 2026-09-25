package metrics

import (
	"context"

	"github.com/hchw/slogan/internal/db"
	"github.com/hchw/slogan/internal/redisx"
)

// PostgresCollector derives request, token, cost/charge, reserve/settle and
// evaluation aggregates from the operational tables. Every label is a bounded
// status/direction string.
type PostgresCollector struct {
	pool *db.Pool
	rdb  *redisx.Client
}

// NewPostgresCollector builds the database and node collector.
func NewPostgresCollector(pool *db.Pool, rdb *redisx.Client) *PostgresCollector {
	return &PostgresCollector{pool: pool, rdb: rdb}
}

// Collect implements Collector.
func (c *PostgresCollector) Collect(ctx context.Context) ([]Sample, error) {
	if c == nil || c.pool == nil {
		return nil, nil
	}
	samples := []Sample{}
	requests, err := c.pool.Query(ctx, `SELECT status, count(*) FROM request_record WHERE created_at > now() - interval '24 hours' GROUP BY status`)
	if err != nil {
		return nil, err
	}
	for requests.Next() {
		var status string
		var count int64
		if err := requests.Scan(&status, &count); err != nil {
			requests.Close()
			return nil, err
		}
		samples = append(samples, Sample{
			Name: "slogan_gateway_requests_24h", Help: "Gateway requests in the last 24 hours by terminal status.",
			Type: "gauge", Labels: map[string]string{"status": status}, Value: float64(count),
		})
	}
	requests.Close()

	usageRows, err := c.pool.Query(ctx, `SELECT
            COALESCE(sum(request_count),0), COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
            COALESCE(sum(cost_micro),0), COALESCE(sum(charge_micro),0)
        FROM usage_record WHERE stat_date > current_date - 7`)
	if err != nil {
		return nil, err
	}
	if usageRows.Next() {
		var reqs, in, out, cost, charge int64
		if err := usageRows.Scan(&reqs, &in, &out, &cost, &charge); err != nil {
			usageRows.Close()
			return nil, err
		}
		samples = append(samples,
			Sample{Name: "slogan_gateway_tokens_total", Help: "Tokens settled in the last 7 days by direction.", Type: "gauge", Labels: map[string]string{"direction": "input"}, Value: float64(in)},
			Sample{Name: "slogan_gateway_tokens_total", Help: "Tokens settled in the last 7 days by direction.", Type: "gauge", Labels: map[string]string{"direction": "output"}, Value: float64(out)},
			Sample{Name: "slogan_gateway_settled_requests_7d", Help: "Settled requests in the last 7 days.", Type: "gauge", Value: float64(reqs)},
			Sample{Name: "slogan_gateway_cost_micro_7d", Help: "Upstream cost in micro-yuan for the last 7 days.", Type: "gauge", Value: float64(cost)},
			Sample{Name: "slogan_gateway_charge_micro_7d", Help: "User charge in micro-yuan for the last 7 days.", Type: "gauge", Value: float64(charge)},
		)
	}
	usageRows.Close()

	// A reservation is open until the same request records a consume or release.
	var reserved, reservedMicro int64
	if err := c.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(l.amount_micro),0) FROM quota_ledger l
        WHERE l.type='reserve' AND NOT EXISTS (
            SELECT 1 FROM quota_ledger s WHERE s.ref_type=l.ref_type AND s.ref_id=l.ref_id AND s.type IN ('consume','release'))`).
		Scan(&reserved, &reservedMicro); err == nil {
		samples = append(samples,
			Sample{Name: "slogan_gateway_reservations_open", Help: "Quota reservations not yet settled or released.", Type: "gauge", Value: float64(reserved)},
			Sample{Name: "slogan_gateway_reserved_micro", Help: "Micro-yuan currently held as reservations.", Type: "gauge", Value: float64(reservedMicro)},
		)
	}
	var releases, consumes int64
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE type='release'`).Scan(&releases); err == nil {
		samples = append(samples, Sample{Name: "slogan_gateway_reservations_released_total", Help: "Reservations released without a trusted usage.", Type: "counter", Value: float64(releases)})
	}
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM quota_ledger WHERE type='consume'`).Scan(&consumes); err == nil {
		samples = append(samples, Sample{Name: "slogan_gateway_settlements_total", Help: "Settled requests that applied a charge.", Type: "counter", Value: float64(consumes)})
	}

	refreshes, err := c.pool.Query(ctx, `SELECT status, count(*) FROM refresh_task GROUP BY status`)
	if err == nil {
		for refreshes.Next() {
			var status string
			var count int64
			if err := refreshes.Scan(&status, &count); err != nil {
				break
			}
			samples = append(samples, Sample{Name: "slogan_evaluation_tasks", Help: "Evaluation refresh tasks by status.", Type: "gauge", Labels: map[string]string{"status": status}, Value: float64(count)})
		}
		refreshes.Close()
	}
	var evalCost int64
	if err := c.pool.QueryRow(ctx, `SELECT COALESCE(sum(cost_micro),0) FROM refresh_task`).Scan(&evalCost); err == nil {
		samples = append(samples, Sample{Name: "slogan_evaluation_cost_micro_total", Help: "Upstream cost of evaluation runs in micro-yuan.", Type: "counter", Value: float64(evalCost)})
	}

	if c.rdb != nil {
		if nodes, err := c.rdb.Nodes(ctx); err == nil {
			var serving, degraded, stale int
			var active int64
			if v, err := c.rdb.ActiveVersion(ctx); err == nil {
				active = v
			}
			for _, node := range nodes {
				if node.Ready && !node.Drain {
					serving++
				}
				if !node.ClassifierReady {
					degraded++
				}
				if node.Version != active {
					stale++
				}
			}
			samples = append(samples,
				Sample{Name: "slogan_gateway_nodes_serving", Help: "Gateway nodes accepting traffic.", Type: "gauge", Value: float64(serving)},
				Sample{Name: "slogan_gateway_nodes_classifier_degraded", Help: "Gateway nodes serving traffic with an unready classifier.", Type: "gauge", Value: float64(degraded)},
				Sample{Name: "slogan_gateway_nodes_score_version_stale", Help: "Gateway nodes not on the active score version.", Type: "gauge", Value: float64(stale)},
				Sample{Name: "slogan_gateway_score_version_active", Help: "Active score version id (0 when none is published).", Type: "gauge", Value: float64(active)},
			)
		}
	}
	return samples, nil
}
