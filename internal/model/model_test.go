package model

import "testing"

func TestValidateModelInput(t *testing.T) {
	price := int64(1)
	if err := validateInput(Input{ContextLength: -1}); err == nil {
		t.Fatal("negative context length accepted")
	}
	if err := validateInput(Input{InputModalities: []string{"video"}}); err == nil {
		t.Fatal("unknown modality accepted")
	}
	negative := int64(-1)
	if err := validateInput(Input{InputPriceMicro: &negative}); err == nil {
		t.Fatal("negative cost rate accepted")
	}
	if err := validateInput(Input{ChargeOutputMicro: &negative}); err == nil {
		t.Fatal("negative charge rate accepted")
	}
	if err := validateInput(Input{ContextLength: 8192, InputModalities: []string{"text", "image"}, InputPriceMicro: &price}); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
}
