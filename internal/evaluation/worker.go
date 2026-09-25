package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hchw/slogan/internal/audit"
	"github.com/hchw/slogan/internal/provider"
)

const selfWeight = 0.30
const peerWeight = 0.70

// RunWorker consumes evaluation tasks until ctx is cancelled. It recovers
// pending items from the database so a restarted worker never loses work.
func (s *Service) RunWorker(ctx context.Context) {
	_ = s.rdb.EnsureGroup(ctx, StreamName, "evaluators")
	consumer := fmt.Sprintf("worker-%d", time.Now().UnixNano())
	concurrency := s.cfg.Concurrency
	if concurrency <= 0 || concurrency > 5 {
		concurrency = 5
	}
	sem := make(chan struct{}, concurrency)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.recoverPending(ctx)
		msgs, err := s.rdb.Consume(ctx, StreamName, "evaluators", consumer, 5, 2*time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var wg sync.WaitGroup
		for _, message := range msgs {
			m := message
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				taskID := parseInt(m.Values["taskId"])
				modelID := parseInt(m.Values["modelId"])
				if taskID != 0 && modelID != 0 {
					_ = s.ProcessItem(ctx, taskID, modelID)
				}
				_ = s.rdb.Ack(ctx, StreamName, "evaluators", m.ID)
			}()
		}
		wg.Wait()
	}
}

func (s *Service) recoverPending(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT task_id, model_id FROM refresh_task_item
        WHERE status='running' AND started_at < now() - interval '2 minutes' LIMIT 20`)
	if err != nil {
		return
	}
	defer rows.Close()
	type pair struct{ task, model int64 }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.task, &p.model); err == nil {
			pairs = append(pairs, p)
		}
	}
	for _, p := range pairs {
		_, _ = s.pool.Exec(ctx, `UPDATE refresh_task_item SET status='pending' WHERE task_id=$1 AND model_id=$2`, p.task, p.model)
	}
}

// ProcessItem evaluates one participant model within a task.
func (s *Service) ProcessItem(ctx context.Context, taskID, modelID int64) error {
	var itemID int64
	err := s.pool.QueryRow(ctx, `UPDATE refresh_task_item SET status='running', started_at=now()
        WHERE task_id=$1 AND model_id=$2 AND status='pending' RETURNING id`, taskID, modelID).Scan(&itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already claimed
	}
	if err != nil {
		return err
	}

	frozen, versionID, err := s.taskContext(ctx, taskID)
	if err != nil {
		return s.failItem(ctx, taskID, modelID, "task context unavailable")
	}
	target, ok := frozen[modelID]
	if !ok {
		return s.failItem(ctx, taskID, modelID, "model not in frozen participant set")
	}
	targetKey := target.ModelKey

	// Choose an evaluator. Prefer a different participant as peer; use the
	// target itself for the self assessment.
	evalCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	selfResult, err := s.evaluate(evalCtx, frozen[modelID], targetKey)
	if err != nil {
		return s.failItem(ctx, taskID, modelID, classifyEvalError(err))
	}
	peerResult := selfResult
	itemCostMicro := selfResult.CostMicro
	itemOutputTokens := selfResult.OutputTokens
	var peerID int64
	ids := make([]int64, 0, len(frozen))
	for id := range frozen {
		if id != modelID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > 0 {
		peerID = ids[0]
		if peer, err := s.evaluate(evalCtx, frozen[peerID], targetKey); err == nil {
			peerResult = peer
			itemCostMicro += peer.CostMicro
			itemOutputTokens += peer.OutputTokens
		} else {
			// No valid peer output: fall back to the target's self-score and do
			// not persist a fabricated peer evaluation.
			peerID = 0
			peerResult = selfResult
		}
	}
	selfScores, peerScores := selfResult.Capabilities, peerResult.Capabilities

	err = s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		// Raw evaluations (self + peer) for authorized inspection.
		evaluations := []struct {
			evaluator    int64
			weight       float64
			confidence   float64
			outputTokens int
			scores       map[string]float64
		}{{modelID, selfWeight, selfResult.Confidence, selfResult.OutputTokens, selfScores}}
		if peerID != 0 {
			evaluations = append(evaluations, struct {
				evaluator    int64
				weight       float64
				confidence   float64
				outputTokens int
				scores       map[string]float64
			}{peerID, peerWeight, peerResult.Confidence, peerResult.OutputTokens, peerScores})
		}
		for _, es := range evaluations {
			for _, dim := range Dimensions {
				score, ok := es.scores[dim]
				if !ok {
					score = 0
				}
				if _, err := tx.Exec(ctx, `INSERT INTO model_evaluation
                    (refresh_task_id, evaluator_model_id, target_model_id, dimension, score, confidence, weight, output_tokens)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, taskID, es.evaluator, modelID, dim, score, es.confidence, es.weight, es.outputTokens); err != nil {
					return err
				}
			}
		}
		// Aggregated capability (self 30% / peer 70%).
		for _, dim := range Dimensions {
			agg := selfWeight*selfScores[dim] + peerWeight*peerScores[dim]
			confidence := selfWeight*selfResult.Confidence + peerWeight*peerResult.Confidence
			if _, err := tx.Exec(ctx, `INSERT INTO model_capability
                (score_version_id, model_id, dimension, score, self_score, peer_score, confidence)
                VALUES ($1,$2,$3,$4,$5,$6,$7)
                ON CONFLICT (score_version_id, model_id, dimension) DO UPDATE SET
                    score=EXCLUDED.score, self_score=EXCLUDED.self_score, peer_score=EXCLUDED.peer_score, confidence=EXCLUDED.confidence`,
				versionID, modelID, dim, agg, selfScores[dim], peerScores[dim], confidence); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_task_item SET status='succeeded', output_tokens=$2, cost_micro=$3, finished_at=now() WHERE id=$1`, itemID, itemOutputTokens, itemCostMicro); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_task SET succeeded = succeeded + 1, cost_micro=cost_micro+$2 WHERE id=$1`, taskID, itemCostMicro); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.maybeFinalize(ctx, taskID)
}

func (s *Service) failItem(ctx context.Context, taskID, modelID int64, reason string) error {
	_, err := s.pool.Exec(ctx, `UPDATE refresh_task_item SET status='timeout', error_message=$3, finished_at=now()
        WHERE task_id=$1 AND model_id=$2 AND status IN ('pending','running')`, taskID, modelID, reason)
	if err != nil {
		return err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE refresh_task SET failed = failed + 1 WHERE id=$1`, taskID)
	return s.maybeFinalize(ctx, taskID)
}

func (s *Service) taskContext(ctx context.Context, taskID int64) (map[int64]FrozenModel, int64, error) {
	var (
		versionID int64
		frozen    []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT score_version_id, frozen_participants FROM refresh_task WHERE id=$1`, taskID).
		Scan(&versionID, &frozen)
	if err != nil {
		return nil, 0, err
	}
	var raw []FrozenModel
	if err := json.Unmarshal(frozen, &raw); err != nil {
		return nil, 0, err
	}
	out := map[int64]FrozenModel{}
	for _, r := range raw {
		out[r.ID] = r
	}
	return out, versionID, nil
}

// maybeFinalize computes the success ratio once all items are terminal.
func (s *Service) maybeFinalize(ctx context.Context, taskID int64) error {
	var pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM refresh_task_item
        WHERE task_id=$1 AND status IN ('pending','running')`, taskID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	var (
		total     int
		succeeded int
		versionID int64
	)
	if err := s.pool.QueryRow(ctx, `SELECT total, succeeded, score_version_id FROM refresh_task WHERE id=$1`, taskID).
		Scan(&total, &succeeded, &versionID); err != nil {
		return err
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(succeeded) / float64(total)
	}
	err := s.pool.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE score_version SET success_ratio=$2, valid_models=$3 WHERE id=$1`,
			versionID, ratio, succeeded); err != nil {
			return err
		}
		status := "completed"
		if ratio == 0 {
			status = "failed"
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_task SET status=$2, finished_at=now() WHERE id=$1`, taskID, status); err != nil {
			return err
		}
		return s.audit.WriteInTx(ctx, tx, audit.Entry{
			ActorType: "system", Action: "evaluation.complete",
			TargetType: "refresh_task", TargetID: itoa(taskID),
			Detail: map[string]any{"successRatio": ratio, "total": total, "succeeded": succeeded},
		})
	})
	return err
}

// evaluate asks an evaluator model to score a target on all dimensions.
type EvaluationOutput struct {
	Capabilities map[string]float64
	Confidence   float64
	InputTokens  int
	OutputTokens int
	CostMicro    int64
}

func (s *Service) evaluate(ctx context.Context, m FrozenModel, targetKey string) (EvaluationOutput, error) {
	secret, err := s.provider.Secret(ctx, m.ProviderID)
	if err != nil {
		return EvaluationOutput{}, err
	}
	client := provider.NewClient(m.ProviderBaseURL, secret, m.ProviderAuthType, s.cfg.Timeout)

	prompt := buildPrompt(targetKey)
	body, _ := json.Marshal(map[string]any{
		"model":      m.ModelKey,
		"max_tokens": s.cfg.MaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a model capability evaluator. Reply with JSON only."},
			{"role": "user", "content": prompt},
		},
	})
	resp, err := client.Chat(ctx, body)
	if err != nil {
		return EvaluationOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return EvaluationOutput{}, errRateLimited
	}
	if resp.StatusCode/100 != 2 {
		return EvaluationOutput{}, fmt.Errorf("evaluator status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return EvaluationOutput{}, err
	}
	out, err := parseScores(raw)
	if err != nil {
		return EvaluationOutput{}, err
	}
	// Evaluation cost is an operational statistic derived from the frozen
	// upstream prices captured when the refresh started.
	out.CostMicro = int64(out.InputTokens)*m.InputPriceMicro + int64(out.OutputTokens)*m.OutputPriceMicro
	return out, nil
}

var (
	errRateLimited = errors.New("rate_limited")
	errBadOutput   = errors.New("invalid_output")
)

type evalResponse struct {
	EvaluatedModels []struct {
		Capabilities map[string]float64 `json:"capabilities"`
		Confidence   *float64           `json:"confidence"`
	} `json:"evaluatedModels"`
	Capabilities map[string]float64 `json:"capabilities"`
	Confidence   *float64           `json:"confidence"`
}

type providerEvalEnvelope struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func parseScores(raw []byte) (EvaluationOutput, error) {
	text := string(raw)
	inputTokens, outputTokens := 0, 0
	// Provider response is normally an OpenAI chat.completion envelope; parse
	// the assistant's content and usage before validating the evaluation schema.
	var envelope providerEvalEnvelope
	if json.Unmarshal(raw, &envelope) == nil {
		if len(envelope.Choices) > 0 && envelope.Choices[0].Message.Content != "" {
			text = envelope.Choices[0].Message.Content
		}
		if envelope.Usage != nil {
			outputTokens = envelope.Usage.CompletionTokens
			inputTokens = envelope.Usage.PromptTokens
		}
	}
	text = extractJSON(text)
	var out evalResponse
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return EvaluationOutput{}, errBadOutput
	}
	caps := out.Capabilities
	confidence := out.Confidence
	if len(caps) == 0 && len(out.EvaluatedModels) > 0 {
		caps = out.EvaluatedModels[0].Capabilities
		confidence = out.EvaluatedModels[0].Confidence
	}
	if len(caps) == 0 || confidence == nil || *confidence < 0 || *confidence > 1 {
		return EvaluationOutput{}, errBadOutput
	}
	for _, dim := range Dimensions {
		v, ok := caps[dim]
		if !ok {
			return EvaluationOutput{}, errBadOutput
		}
		if err := validateDimension(dim, v); err != nil {
			return EvaluationOutput{}, err
		}
	}
	for dim, v := range caps {
		if err := validateDimension(dim, v); err != nil {
			return EvaluationOutput{}, err
		}
	}
	return EvaluationOutput{Capabilities: caps, Confidence: *confidence, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
}

func validateDimension(dim string, v float64) error {
	known := false
	for _, d := range Dimensions {
		if d == dim {
			known = true
			break
		}
	}
	if !known {
		return errBadOutput
	}
	if v < 0 || v > 1 {
		return errBadOutput
	}
	return nil
}

// extractJSON pulls the first JSON object from a possibly fenced response.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func buildPrompt(targetKey string) string {
	return fmt.Sprintf(`Evaluate the chat model %q on a 0..1 scale for each dimension: %s.
Return JSON: {"evaluatedModels":[{"modelId":%q,"capabilities":{"coding":0.0,...},"confidence":0.0}]}.`,
		targetKey, strings.Join(Dimensions, ", "), targetKey)
}

func classifyEvalError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errRateLimited):
		return "rate_limited"
	case errors.Is(err, errBadOutput):
		return "invalid_output"
	default:
		return "unavailable"
	}
}
