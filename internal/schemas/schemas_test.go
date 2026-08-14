package schemas

import "testing"

const validCorrectionResult = `{
  "corrections": [
    {
      "original": "面白いでした",
      "replacement": "面白かったです",
      "type": "conjugation",
      "severity": "incorrect",
      "explanation": {"ja": "説明", "en": "explanation"},
      "concepts": ["i-adjective-past"]
    }
  ]
}`

const emptyCorrectionResult = `{"corrections": []}`

func TestGetReturnsNonEmptySchema(t *testing.T) {
	sch, err := Get("correction_result.v1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if len(sch) == 0 {
		t.Fatal("Get returned empty schema")
	}
}

func TestGetUnknownNameErrors(t *testing.T) {
	if _, err := Get("no.such.schema"); err == nil {
		t.Fatal("expected error for unknown schema name, got nil")
	}
}

func TestValidateGoodDoc(t *testing.T) {
	if err := Validate("correction_result.v1", []byte(validCorrectionResult)); err != nil {
		t.Fatalf("Validate(good doc) returned error: %v", err)
	}
	if err := Validate("correction_result.v1", []byte(emptyCorrectionResult)); err != nil {
		t.Fatalf("Validate(empty corrections) returned error: %v", err)
	}
}

func TestValidateMissingSeverityFails(t *testing.T) {
	doc := `{
  "corrections": [
    {
      "original": "面白いでした",
      "replacement": "面白かったです",
      "type": "conjugation",
      "explanation": {"ja": "説明", "en": "explanation"}
    }
  ]
}`
	if err := Validate("correction_result.v1", []byte(doc)); err == nil {
		t.Fatal("expected error for missing severity, got nil")
	}
}

func TestValidateUnknownTypeEnumFails(t *testing.T) {
	doc := `{
  "corrections": [
    {
      "original": "面白いでした",
      "replacement": "面白かったです",
      "type": "not-a-real-type",
      "severity": "incorrect",
      "explanation": {"ja": "説明", "en": "explanation"}
    }
  ]
}`
	if err := Validate("correction_result.v1", []byte(doc)); err == nil {
		t.Fatal("expected error for unknown type enum value, got nil")
	}
}

func TestValidateUnknownSchemaNameErrors(t *testing.T) {
	if err := Validate("no.such.schema", []byte(emptyCorrectionResult)); err == nil {
		t.Fatal("expected error for unknown schema name, got nil")
	}
}
