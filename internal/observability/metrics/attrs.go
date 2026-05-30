package metrics

import "go.opentelemetry.io/otel/attribute"

// attrStr is a thin local alias so the instrument call sites stay terse.
func attrStr(key, value string) attribute.KeyValue {
	return attribute.String(key, value)
}
