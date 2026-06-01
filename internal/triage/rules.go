package triage

import "strings"

// registry is the compiled rule set for one triage run, merging built-ins
// with per-project overrides from .osmo.json.
type registry struct {
	flagResources map[string]string // resource type → human reason
	flagAttrs     []string          // substring patterns → Flag
	reviewAttrs   []string          // substring patterns → Review
	safeAttrs     []string          // substring patterns → Safe (overrides flag/review)
}

// builtinFlagResources maps Terraform resource type names to the human-readable
// reason they are considered security-sensitive by default.
var builtinFlagResources = map[string]string{
	// AWS — network
	"aws_security_group":                  "controls network ingress/egress",
	"aws_security_group_rule":             "controls network ingress/egress",
	"aws_vpc":                             "core network configuration",
	"aws_vpc_endpoint":                    "controls VPC data path",
	"aws_network_acl":                     "network access control list",
	"aws_network_acl_rule":               "network access control list rule",
	// AWS — IAM
	"aws_iam_policy":                      "controls IAM permissions",
	"aws_iam_role_policy":                 "controls IAM permissions",
	"aws_iam_role_policy_attachment":      "controls IAM permissions",
	"aws_iam_user_policy":                 "controls IAM permissions",
	"aws_iam_user_policy_attachment":      "controls IAM permissions",
	"aws_iam_group_policy":               "controls IAM permissions",
	// AWS — storage access
	"aws_s3_bucket_acl":                   "controls S3 public access",
	"aws_s3_bucket_public_access_block":   "controls S3 public access",
	"aws_s3_bucket_policy":               "controls S3 bucket permissions",
	// AWS — encryption
	"aws_kms_key":                         "encryption key configuration",
	"aws_kms_grant":                       "encryption key grant",
	"aws_kms_key_policy":                  "encryption key policy",
	// AWS — secrets
	"aws_secretsmanager_resource_policy":  "controls secrets access",
	// Azure — network
	"azurerm_network_security_group":      "controls network ingress/egress",
	"azurerm_network_security_rule":       "controls network ingress/egress",
	// Azure — IAM
	"azurerm_role_assignment":             "controls RBAC permissions",
	"azurerm_role_definition":             "controls RBAC role definition",
	"azurerm_key_vault_access_policy":     "controls key vault permissions",
	// GCP — network
	"google_compute_firewall":             "controls network ingress/egress",
	// GCP — IAM
	"google_iam_binding":                  "controls IAM permissions",
	"google_project_iam_member":           "controls IAM permissions",
	"google_project_iam_binding":          "controls IAM permissions",
}

// builtinFlagAttrPatterns are substring patterns matched against attribute
// names. Any match upgrades the attribute's verdict to Flag.
var builtinFlagAttrPatterns = []string{
	"cidr", "ingress", "egress",
	"policy", "principal", "trust_relationship",
	"public_access", "acl", "bucket_policy",
	"kms_key", "encryption_key",
	"from_port", "to_port",
	"security_group", "firewall_rule",
}

// builtinReviewAttrPatterns are patterns that suggest the attribute is managed
// by an external controller (autoscaler, scheduler). Absorbing a literal may
// fight the controller; lifecycle.ignore_changes is often the right fix.
var builtinReviewAttrPatterns = []string{
	"desired_capacity", "desired_count",
	"min_elb_capacity",
	"replica_count", "node_count", "instance_count",
}

// builtinSafeAttrPatterns are always Safe regardless of resource type.
// They override flag/review pattern matches.
var builtinSafeAttrPatterns = []string{
	"tag", "label", "description",
	"annotation", "comment", "display_name",
}

func buildRegistry(cfg Config) *registry {
	fr := make(map[string]string, len(builtinFlagResources))
	for k, v := range builtinFlagResources {
		fr[k] = v
	}
	for _, r := range cfg.FlagResources {
		fr[r] = "flagged in .osmo.json triage config"
	}

	return &registry{
		flagResources: fr,
		flagAttrs:     append(builtinFlagAttrPatterns, cfg.FlagAttrs...),
		reviewAttrs:   builtinReviewAttrPatterns,
		safeAttrs:     append(builtinSafeAttrPatterns, cfg.SafeAttrs...),
	}
}

// classifyAttr returns the Severity and human reason for one attribute name.
func (r *registry) classifyAttr(attr string) (Severity, string) {
	lower := strings.ToLower(attr)

	// Safe patterns override all others.
	for _, p := range r.safeAttrs {
		if strings.Contains(lower, p) {
			return Safe, ""
		}
	}
	for _, p := range r.flagAttrs {
		if strings.Contains(lower, p) {
			return Flag, "matches security-sensitive pattern '" + p + "'"
		}
	}
	for _, p := range r.reviewAttrs {
		if strings.Contains(lower, p) {
			return Review, "capacity/autoscaler attribute — may be externally managed"
		}
	}
	return Safe, ""
}
