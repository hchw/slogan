package routing

import "regexp"

// High-risk rule version. Rules are deterministic and versioned; they never
// depend on the Laya classifier.
const highRiskRuleVersion = "v1"

type highRiskRule struct {
	name string
	re   *regexp.Regexp
}

// Patient/user-facing intent hints in Chinese and English. These rules only
// gate the quality-first policy; they do not select a model by themselves.
var highRiskRules = []highRiskRule{
	{"security", regexp.MustCompile(`(?i)(漏洞|攻击|渗透|exploit|vulnerabilit|security\s+audit|SQL\s*注入|XSS|越权)`)},
	{"destructive_db", regexp.MustCompile(`(?i)(数据库迁移|删除.*(表|库)|drop\s+table|truncate\s+table|破坏性数据|数据清空|migration\s+rollback)`)},
	{"legal", regexp.MustCompile(`(?i)(法律意见|诉讼|合同纠纷|legal\s+advice|litigation|lawyer)`)},
	{"medical", regexp.MustCompile(`(?i)(医疗诊断|诊断结果|用药建议|medical\s+diagnosis|prescription|dosage)`)},
}

// HighRisk reports whether any versioned rule matches the request text.
func HighRisk(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range highRiskRules {
		if r.re.MatchString(text) {
			return true
		}
	}
	return false
}
