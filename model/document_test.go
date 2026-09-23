package model

import (
	"testing"
)

type metadataDocument struct {
	Document
	metadata map[string]any
}

func (d metadataDocument) Metadata() map[string]any { return d.metadata }

func TestPublicMetadataHidesReservedKeys(t *testing.T) {
	doc := metadataDocument{metadata: map[string]any{"type": "contrat", "_acl": []any{"entra:user:1"}}}

	public := PublicMetadata(doc)
	if _, found := public["_acl"]; found {
		t.Errorf("reserved key exposed: %v", public)
	}
	if public["type"] != "contrat" {
		t.Errorf("public key dropped: %v", public)
	}

	if got := PublicMetadata(metadataDocument{metadata: map[string]any{"_acl": []any{}}}); got != nil {
		t.Errorf("expected nil when only reserved keys remain, got %v", got)
	}
}
