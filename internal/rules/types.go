package rules

import "encoding/json"

// APIVersion and Kind are the only accepted values for the rules file header.
const (
	APIVersion = "statuspatcher.dev/v1alpha1"
	Kind       = "GreenlightConfig"

	// Domain is the annotation domain used by the shipped example rules.
	Domain = "statuspatcher.dev"
)

// Reserved status field names that rules must not produce themselves.
const (
	conditionsField         = "conditions"
	observedGenerationField = "observedGeneration"
)

// fileSpec is the on-disk representation of a rules file.
type fileSpec struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Params     json.RawMessage `json:"params,omitempty"`
	Defaults   *defaultsSpec   `json:"defaults,omitempty"`
	Rules      []ruleSpec      `json:"rules,omitempty"`
}

type defaultsSpec struct {
	ReadyCondition *readyDefaultsSpec `json:"readyCondition,omitempty"`
}

type readyDefaultsSpec struct {
	Reason      *string `json:"reason,omitempty"`
	FalseReason *string `json:"falseReason,omitempty"`
	Message     *string `json:"message,omitempty"`
}

type ruleSpec struct {
	Name            string              `json:"name"`
	Match           matchSpec           `json:"match"`
	MatchConditions []matchConditionDef `json:"matchConditions,omitempty"`
	Delay           string              `json:"delay,omitempty"`
	Variables       []variableSpec      `json:"variables,omitempty"`
	Status          map[string]string   `json:"status,omitempty"`
	ReadyCondition  *conditionSpec      `json:"readyCondition,omitempty"`
	Conditions      []conditionSpec     `json:"conditions,omitempty"`
}

type matchSpec struct {
	APIGroup string   `json:"apiGroup"`
	Kinds    []string `json:"kinds"`
	Versions []string `json:"versions,omitempty"`
}

type matchConditionDef struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type variableSpec struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type conditionSpec struct {
	Type              string  `json:"type,omitempty"`
	StatusExpression  string  `json:"statusExpression,omitempty"`
	Reason            *string `json:"reason,omitempty"`
	FalseReason       *string `json:"falseReason,omitempty"`
	Message           *string `json:"message,omitempty"`
	MessageExpression string  `json:"messageExpression,omitempty"`
}
