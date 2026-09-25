package evaluation

import (
	"encoding/json"
	"testing"
)

func scoreContent(confidence string) string {
	caps := map[string]float64{}
	for _, d := range Dimensions {
		caps[d] = 0.75
	}
	capsJSON, _ := json.Marshal(caps)
	return `{"capabilities":` + string(capsJSON) + `,"confidence":` + confidence + `}`
}

func TestParseScoresValidatesOpenAIEnvelopeAndDimensions(t *testing.T) {
	content := scoreContent("0.87")
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
		"usage":   map[string]int{"completion_tokens": 123},
	})
	got, err := parseScores(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != len(Dimensions) || got.Confidence != 0.87 || got.OutputTokens != 123 {
		t.Fatalf("evaluation=%+v", got)
	}
}

func TestParseScoresRejectsMissingOrInvalidConfidenceAndDimensions(t *testing.T) {
	for _, content := range []string{
		`{"capabilities":{"coding":0.7},"confidence":0.9}`,
		`{"capabilities":{"coding":1.1},"confidence":0.9}`,
		`{"capabilities":{"coding":0.7}}`,
		`not json`,
	} {
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
		if _, err := parseScores(body); err == nil {
			t.Fatalf("invalid evaluation accepted: %s", content)
		}
	}
}
