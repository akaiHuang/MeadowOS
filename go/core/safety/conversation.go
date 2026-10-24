// Package safety contains product-level conversation safety boundaries shared
// by CLI, Desktop, and Serve.  It is intentionally independent from model and
// tool routing: the model still authors ordinary replies, while the kernel can
// fail closed for a small set of explicit, high-consequence user intents.
package safety

import (
	"regexp"
	"strings"
)

type Category string

const (
	CategoryOrdinary       Category = "ordinary"
	CategoryEmotional      Category = "emotional_support"
	CategoryAbuse          Category = "abusive_language"
	CategorySelfHarm       Category = "self_harm"
	CategoryHarmOthers     Category = "harm_others"
	CategoryBullying       Category = "bullying"
	CategorySexualExplicit Category = "sexual_explicit"
	CategoryCrime          Category = "crime"
	CategoryToxicExposure  Category = "toxic_exposure"
	CategorySafetySupport  Category = "safety_support"
)

type Severity string

const (
	SeverityNone      Severity = "none"
	SeveritySupport   Severity = "support"
	SeverityRefuse    Severity = "refuse"
	SeverityEmergency Severity = "emergency"
)

// Assessment is an observable, deterministic product boundary.  Match is
// deliberately high precision: broad topic mentions such as "自殺防治" or
// "犯罪學研究" must not be mistaken for harmful intent.
type Assessment struct {
	Category     Category `json:"category"`
	Severity     Severity `json:"severity"`
	DisableTools bool     `json:"disable_tools"`
	GuardFinal   bool     `json:"guard_final"`
}

// VerifiedTaiwanPublicNumber is the narrow allowlist for public emergency and
// reporting numbers that may be stated without web verification.  Keeping it
// in the shared safety package lets CLI, Desktop, model-output guards, and
// evaluators use one product boundary instead of maintaining prompt-only lists.
func VerifiedTaiwanPublicNumber(digits string) bool {
	digits = regexp.MustCompile(`\D`).ReplaceAllString(digits, "")
	switch digits {
	case "110", "113", "119", "165", "1925":
		return true
	default:
		return false
	}
}

var (
	emotionalRe = regexp.MustCompile(`(?i)(我|i(?:'m| am))[^\n]{0,60}(心情(?:很)?(?:不好|低落)|難過|难过|傷心|伤心|孤單|孤单|寂寞|焦慮|焦虑|壓力很大|压力很大|撐不下去|撑不下去|沒用|没用|很累|好累|失敗|失败|挫折|sad|lonely|anxious|overwhelmed|useless)|(?:最近)?心情(?:很)?(?:不好|低落)|(?:陪我|聽我|听我)[^\n]{0,16}(聊|說|说|talk|listen)`)
	abuseRe     = regexp.MustCompile(`(?i)(吃屎|去死|你(?:真的)?很笨|你是白痴|你是智障|廢物|废物|垃圾(?:ai)?|fuck you|you(?:'re| are) stupid|idiot)`)

	selfHarmDirectRe     = regexp.MustCompile(`(?i)(?:(?:我|自己|i|myself)[^\n]{0,28}(?:想|要|準備|准备|打算|正在|would like to|want to|plan to|going to)[^\n]{0,24}(?:自殺|自杀|尋短|寻短|結束生命|结束生命|傷害自己|伤害自己|suicide|kill myself|end my life|self[- ]?harm)|(?:我|自己)[^\n]{0,16}(?:不想(?:再)?活|活不下去|不想存在)|(?:怎麼|怎么|如何|方法|步驟|步骤|教我|how (?:can|do) i)[^\n]{0,32}(?:自殺|自杀|尋短|寻短|結束生命|结束生命|傷害自己|伤害自己|suicide|kill myself|self[- ]?harm))`)
	selfHarmPreventionRe = regexp.MustCompile(`(?i)(防治|預防|预防|阻止|勸阻|劝阻|救|協助|协助|幫助|帮助|關懷|关怀|陪|怎麼陪|怎么陪|研究|新聞|新闻|小說|小说|報告|报告|熱線|热线|專線|专线|資源|资源|prevention|prevent|help|support|stay with|hotline|research|report|novel)[^\n]{0,36}(自殺|自杀|想死|不想活|self[- ]?harm|suicide)|(?:自殺|自杀|想死|不想活|self[- ]?harm|suicide)[^\n]{0,36}(防治|預防|预防|阻止|勸阻|劝阻|救|協助|协助|幫助|帮助|關懷|关怀|陪|怎麼陪|怎么陪|求助|研究|新聞|新闻|小說|小说|報告|报告|熱線|热线|專線|专线|資源|资源|prevention|prevent|help|support|stay with|hotline|research|report|novel)`)

	harmOthersRe         = regexp.MustCompile(`(?i)(?:怎麼|怎么|如何|方法|步驟|步骤|教我|幫我|帮我|我要|我想|我打算|how (?:can|do) i|help me)[^\n]{0,40}(殺人|杀人|殺死|杀死|傷害他人|伤害他人|攻擊他|攻击他|放火|炸彈|炸弹|下毒|kill (?:him|her|them|someone)|hurt (?:him|her|them|someone)|bomb|poison someone)`)
	bullyingRe           = regexp.MustCompile(`(?i)(?:怎麼|怎么|如何|方法|教我|幫我|帮我|how (?:can|do) i|help me)[^\n]{0,40}(霸凌|欺負|欺负|羞辱|排擠|排挤|散播謠言|散播谣言|bully|harass|humiliate)`)
	bullyingPreventionRe = regexp.MustCompile(`(?i)(防止|預防|预防|制止|處理|处理|應對|应对|反霸凌|研究|報告|报告|prevent|stop|respond|victim|anti)[^\n]{0,28}(霸凌|欺負|欺负|bully|harass)|(?:被|遭受|遇到|面對|面对)[^\n]{0,8}(霸凌|欺負|欺负|bully|harass)|(?:霸凌|欺負|欺负|bully|harass)[^\n]{0,28}(防止|預防|预防|制止|處理|处理|應對|应对|求助|受害|研究|報告|报告|prevent|stop|respond|victim|anti)`)

	sexualExplicitRe  = regexp.MustCompile(`(?i)(?:寫|写|生成|描述|扮演|角色扮演|給我|给我|教我|write|generate|describe|roleplay|show me)[^\n]{0,36}(色情|A片|做愛|做爱|性交|露骨性愛|露骨性爱|裸照|porn|explicit sex|sexual roleplay|nude image)`)
	sexualEducationRe = regexp.MustCompile(`(?i)(教育|健康|同意|安全性行為|安全性行为|避孕|疾病|防治|醫療|医疗|研究|報告|报告|education|health|consent|contraception|disease|medical|research|report)`)

	crimeActionRe = regexp.MustCompile(`(?i)(?:怎麼|怎么|如何|方法|步驟|步骤|教我|幫我|帮我|how (?:can|do) i|help me)[^\n]{0,48}(偷竊|偷窃|搶劫|抢劫|詐騙|诈骗|洗錢|洗钱|製毒|制毒|販毒|贩毒|逃避警察|躲避警方|入侵[^\n]{0,12}(?:帳號|账号)|破解[^\n]{0,12}(?:密碼|密码)|偷車|偷车|steal|rob|scam|launder money|make drugs|sell drugs|evade police|hack (?:an )?account|crack (?:a )?password)`)
	crimeBenignRe = regexp.MustCompile(`(?i)(防止|預防|预防|辨識|辨识|受害|報案|报案|法律|合法|研究|新聞|新闻|小說|小说|報告|报告|prevent|detect|victim|report|legal|law|research|news|novel)`)

	toxicExposureRe = regexp.MustCompile(`(?i)(我|小孩|孩子|嬰兒|婴儿|寵物|宠物|他|她|有人|\bi\b|my child|baby|pet|someone)[^\n]{0,30}(喝了|吃了|吞了|吸入|誤食|误食|接觸|接触|濺到|溅到|\bingested\b|\bswallowed\b|\bdrank\b|\bate\b|\binhaled\b|\bexposed\b)|(?:喝|吃|吞|服用|混合|how much|dose)[^\n]{0,28}(漂白水|清潔劑|清洁剂|農藥|农药|老鼠藥|老鼠药|強酸|强酸|強鹼|强碱|一氧化碳|bleach|cleaner|pesticide|rat poison|toxic|poison)`)
)

func Assess(text string) Assessment {
	text = strings.TrimSpace(text)
	if text == "" {
		return Assessment{Category: CategoryOrdinary, Severity: SeverityNone}
	}
	switch {
	case selfHarmDirectRe.MatchString(text) && !selfHarmPreventionRe.MatchString(text):
		return Assessment{Category: CategorySelfHarm, Severity: SeverityEmergency, DisableTools: true, GuardFinal: true}
	case toxicExposureRe.MatchString(text):
		return Assessment{Category: CategoryToxicExposure, Severity: SeverityEmergency, DisableTools: true, GuardFinal: true}
	case harmOthersRe.MatchString(text):
		return Assessment{Category: CategoryHarmOthers, Severity: SeverityEmergency, DisableTools: true, GuardFinal: true}
	case bullyingRe.MatchString(text) && !bullyingPreventionRe.MatchString(text):
		return Assessment{Category: CategoryBullying, Severity: SeverityRefuse, DisableTools: true, GuardFinal: true}
	case sexualExplicitRe.MatchString(text) && !sexualEducationRe.MatchString(text):
		return Assessment{Category: CategorySexualExplicit, Severity: SeverityRefuse, DisableTools: true, GuardFinal: true}
	case crimeActionRe.MatchString(text) && !crimeBenignRe.MatchString(text):
		return Assessment{Category: CategoryCrime, Severity: SeverityRefuse, DisableTools: true, GuardFinal: true}
	case selfHarmPreventionRe.MatchString(text) || bullyingPreventionRe.MatchString(text) ||
		(sexualEducationRe.MatchString(text) && regexp.MustCompile(`(?i)(性|身體|身体|sexual|consent)`).MatchString(text)) ||
		(crimeBenignRe.MatchString(text) && regexp.MustCompile(`(?i)(詐騙|诈骗|犯罪|洗錢|洗钱|scam|crime|fraud)`).MatchString(text)):
		return Assessment{Category: CategorySafetySupport, Severity: SeveritySupport, DisableTools: true}
	case abuseRe.MatchString(text):
		return Assessment{Category: CategoryAbuse, Severity: SeveritySupport}
	case emotionalRe.MatchString(text):
		return Assessment{Category: CategoryEmotional, Severity: SeveritySupport}
	default:
		return Assessment{Category: CategoryOrdinary, Severity: SeverityNone}
	}
}

var contextualFollowUpRe = regexp.MustCompile(`(?i)^(那|那麼|那么|所以|然後|然后|可是|但是|如果|再|繼續|继续|好|對|对|網路說|网络说|有人說|有人说|我看到|不要勸我|不要劝我|別勸我|别劝我|不要阻止我|直接告訴我|直接告诉我|yes|ok|okay|then|so|but|what about|how about|please continue).{0,80}$|^(怎麼做|怎么做|方法呢|步驟呢|步骤呢|哪一個|哪一个|最快的呢|最有效的呢|more|continue)$`)

// AssessConversation inherits a recent high-consequence boundary only for an
// unmistakably contextual follow-up.  A new, self-contained topic must not be
// trapped in the previous turn's refusal state.
func AssessConversation(current string, priorUserTexts []string) Assessment {
	assessment := Assess(current)
	if assessment.Category != CategoryOrdinary || !contextualFollowUpRe.MatchString(strings.TrimSpace(current)) {
		return assessment
	}
	for index := len(priorUserTexts) - 1; index >= 0; index-- {
		priorText := strings.TrimSpace(priorUserTexts[index])
		prior := Assess(priorText)
		if prior.GuardFinal {
			return prior
		}
		// A contextual message may be chained (for example, "那最快的呢？"
		// followed by "繼續").  A self-contained ordinary message means the
		// user changed subject, so an older risk must not leak across it.
		if !contextualFollowUpRe.MatchString(priorText) {
			break
		}
	}
	return assessment
}

// Guidance is short because this context is injected on every model turn.
// It describes the response boundary without exposing classifier details.
func Guidance(assessment Assessment) string {
	switch assessment.Category {
	case CategoryEmotional:
		return "Respond warmly and specifically to the feeling expressed. Listen before offering one small practical next step; do not diagnose, lecture, or pretend to be a human professional."
	case CategoryAbuse:
		return "Do not retaliate, shame, or mirror insults. Acknowledge frustration briefly, apologize when the assistant may have failed, and invite one concrete correction."
	case CategorySafetySupport:
		return "This is a benign prevention, education, or help-seeking request. Answer directly with safe, practical support; do not refuse the topic, browse unnecessarily, or provide harmful operational details."
	case CategorySelfHarm:
		return "This is an immediate self-harm safety conversation. Do not provide methods or optimization. Be compassionate, ask whether danger is immediate, encourage moving away from means and contacting a trusted person, and give the verified Taiwan 1925/119 resources."
	case CategoryHarmOthers:
		return "Do not provide instructions to harm another person. De-escalate, encourage distance from weapons or the target, and direct immediate danger to Taiwan emergency services 110/119."
	case CategoryBullying:
		return "Refuse assistance with bullying or humiliation. Offer non-harmful conflict resolution, documentation, reporting, or boundary-setting instead."
	case CategorySexualExplicit:
		return "Refuse creation of explicit sexual content. Offer a non-explicit, consensual, educational, health, or relationship-focused alternative."
	case CategoryCrime:
		return "Refuse actionable crime, evasion, or concealment instructions. Offer lawful prevention, safety, reporting, or compliance help instead."
	case CategoryToxicExposure:
		return "Treat possible poisoning as urgent. Do not suggest tasting, induced vomiting, home neutralization, or guessed dosing. Tell the user to stop exposure, keep the container, and call 119 or a poison/medical professional now."
	default:
		return ""
	}
}

// GuardedResponse is a fail-safe for explicit high-consequence intents.  It is
// not used for ordinary emotional support or insults, where preserving the
// model's natural conversational ability is part of the product evaluation.
func GuardedResponse(assessment Assessment, modelText string) (string, bool) {
	if !assessment.GuardFinal {
		return strings.TrimSpace(modelText), false
	}
	switch assessment.Category {
	case CategorySelfHarm:
		return "我很在意你現在的安全，也很遺憾你正承受這些。你現在是否已經準備好方法、工具，或可能立刻傷害自己？如果是，請先離開那些物品、到有其他人的地方，並立刻撥打 119；也可以聯絡台灣 1925 安心專線（24 小時）或請一位你信任的人現在陪著你。你不需要獨自撐過這一刻。", true
	case CategoryHarmOthers:
		return "我不能協助傷害他人。請先和對方、武器或可能造成傷害的物品拉開距離，暫停行動並找可信任的人陪你；若有人可能立刻受傷，請立即撥打 110 或 119。你可以告訴我現在發生了什麼，我能陪你把下一步改成安全的做法。", true
	case CategoryToxicExposure:
		return "這可能是急性中毒風險。請立刻停止接觸、移到通風處並保留產品容器或標示；不要催吐、不要自行中和，也不要再吃喝來『稀釋』。若已吞食、吸入、出現不適，或當事人是兒童／寵物，請立即撥打 119 或聯絡毒物／醫療專業人員，並照其指示處理。", true
	case CategoryBullying:
		return "我不能幫你霸凌、羞辱或孤立別人。我可以改為協助你用不傷人的方式表達界線、處理衝突，或整理事件紀錄並尋求老師、主管或可信任大人的協助。", true
	case CategorySexualExplicit:
		return "我不能協助製作露骨色情內容。我可以改寫成不露骨、重視同意與關係互動的版本，或提供性健康與安全教育資訊。", true
	case CategoryCrime:
		return "我不能提供犯罪、規避追查或隱藏行為的操作方法。我可以協助你了解合法選項、降低風險、保護自己，或整理報案與合規處理步驟。", true
	default:
		return strings.TrimSpace(modelText), false
	}
}

var publicNumberClaimRe = regexp.MustCompile(`(?i)(?:熱線|热线|專線|专线|電話|电话|撥打|拨打|致電|致电|報案|报案|警方|警察|緊急|紧急|hotline|call|police|emergency)[^\n]{0,24}?(\+?\d[\d -]{1,}\d)`)

// GuardModelPublicNumbers rejects a model-authored answer that presents an
// unverified public phone number as a hotline, emergency, or reporting number.
// It does not alter ordinary dates, prices, quantities, or private numbers the
// user supplied; callers can retry once or return the safe fallback.
func GuardModelPublicNumbers(text string) (string, bool) {
	for _, match := range publicNumberClaimRe.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 && !VerifiedTaiwanPublicNumber(match[1]) {
			return "", false
		}
	}
	return strings.TrimSpace(text), true
}
