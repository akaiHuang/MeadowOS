package agent

import (
	"strings"
	"unicode"
)

// SearchTaskKind is a product-level retrieval shape, not an answer oracle.
// It controls bounded transport, evidence, and answer budgets while leaving
// the actual query, source contents, and claims to the research loop.
type SearchTaskKind string

const (
	SearchTaskCurrentFact          SearchTaskKind = "current_fact"
	SearchTaskLocalRecommendation  SearchTaskKind = "local_recommendation"
	SearchTaskProductPurchase      SearchTaskKind = "product_purchase"
	SearchTaskEventTravel          SearchTaskKind = "event_travel"
	SearchTaskComparison           SearchTaskKind = "comparison"
	SearchTaskSoftwareGuide        SearchTaskKind = "software_guide"
	SearchTaskKnowledgeExplanation SearchTaskKind = "knowledge_explanation"
	SearchTaskHighRisk             SearchTaskKind = "high_risk"
	SearchTaskOpenResearch         SearchTaskKind = "open_research"
)

// SearchRAGCard is the compact, deterministic search-method card supplied to
// a small model. It says how much evidence is useful and how to judge source
// sufficiency; it never contains topic facts or a prewritten conclusion.
type SearchRAGCard struct {
	Kind                SearchTaskKind `json:"kind"`
	CandidateLimit      int            `json:"candidate_limit"`
	MinDeepReads        int            `json:"min_deep_reads"`
	MaxDeepReads        int            `json:"max_deep_reads"`
	EvidenceTokenBudget int            `json:"evidence_token_budget"`
	AnswerTokenBudget   int            `json:"answer_token_budget"`
	RequiredFields      []string       `json:"required_fields"`
	SourcePriority      []string       `json:"source_priority"`
	Freshness           string         `json:"freshness"`
	StopRule            string         `json:"stop_rule"`
	AnswerFormat        string         `json:"answer_format"`
}

// ClassifySearchRAGCard uses only broad request-shape markers. The returned
// card is deliberately reusable across topics so it cannot encode the answer
// to a benchmark item or overfit a named website.
func ClassifySearchRAGCard(task string) SearchRAGCard {
	lower := strings.ToLower(strings.TrimSpace(task))
	card := openResearchSearchRAGCard()

	switch {
	case containsAnySearchMarker(lower,
		"醫療", "健康", "症狀", "藥物", "診斷", "法律", "法規", "合約", "訴訟", "金融", "投資", "稅務", "保險",
		"medical", "health", "symptom", "medicine", "legal", "law", "contract", "financial", "investment", "tax", "insurance"):
		card.Kind = SearchTaskHighRisk
		card.MinDeepReads, card.MaxDeepReads = 2, 3
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1200, 240
		card.RequiredFields = []string{"適用地區", "發布或更新日期", "適用條件", "風險與限制"}
		card.SourcePriority = []string{"主管機關或官方文件", "專業或原始來源", "可信二手解說"}
		card.Freshness = "必須保留適用地區與更新日期；過期或無日期資料不得覆蓋新資料"
		card.StopRule = "至少兩個高品質來源交叉支持；衝突或缺口必須明示"
		card.AnswerFormat = "結論、適用條件、風險、來源與尚未確認事項"
	case containsAnySearchMarker(lower,
		"餐廳", "餐馆", "景點", "景点", "附近", "在地", "住宿", "旅館", "飯店", "好吃",
		"restaurant", "nearby", "local", "attraction", "hotel", "where to eat"):
		card.Kind = SearchTaskLocalRecommendation
		card.MinDeepReads, card.MaxDeepReads = 3, 4
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1000, 240
		card.RequiredFields = []string{"名稱", "類型", "地址", "營業時間", "價位", "近期營業狀態"}
		card.SourcePriority = []string{"官方店家或場館資料", "近期地圖或平台資料", "近期評論或編輯內容"}
		card.Freshness = "地址、營業時間與營業狀態優先使用近期資料"
		card.StopRule = "候選的必要欄位足夠比較，且官方事實與主觀評價分開"
		card.AnswerFormat = "候選表格、選擇理由、改選條件與未確認欄位"
	case containsAnySearchMarker(lower,
		"商品", "產品", "型號", "售價", "價格", "購買", "购买", "庫存", "规格", "規格", "保固", "哪裡買",
		"product", "model", "price", "buy", "purchase", "stock", "specification", "warranty"):
		card.Kind = SearchTaskProductPurchase
		card.MinDeepReads, card.MaxDeepReads = 2, 4
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1200, 320
		card.RequiredFields = []string{"型號", "規格", "官方售價", "通路價格", "庫存日期", "保固或限制"}
		card.SourcePriority = []string{"官方產品頁或規格", "授權通路", "獨立測試來源"}
		card.Freshness = "價格與庫存必須附查詢日期"
		card.StopRule = "官方規格已確認，價格或體驗至少有一個獨立來源"
		card.AnswerFormat = "規格與價格表格、適合對象、改選條件"
	case containsAnySearchMarker(lower,
		"活動", "展覽", "演唱會", "旅遊", "旅游", "行程", "門票", "票價", "交通", "開放時間",
		"event", "exhibition", "concert", "travel", "itinerary", "ticket", "transport"):
		card.Kind = SearchTaskEventTravel
		card.MinDeepReads, card.MaxDeepReads = 2, 4
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1000, 240
		card.RequiredFields = []string{"日期", "地點", "費用", "交通", "限制", "官方頁面"}
		card.SourcePriority = []string{"主辦或場館官方頁", "交通或政府資料", "可信旅遊資訊"}
		card.Freshness = "日期、票價與限制以最新官方公告為準"
		card.StopRule = "時間、地點、費用與限制皆已確認或明示缺口"
		card.AnswerFormat = "重點表格、行動建議、限制與來源"
	case containsAnySearchMarker(lower,
		"軟體", "软件", "教學", "教程", "安裝", "安装", "設定", "配置", "api", "sdk", "版本", "錯誤", "报错", "bug",
		"software", "tutorial", "install", "setup", "configure", "version", "error", "documentation"):
		card.Kind = SearchTaskSoftwareGuide
		card.MinDeepReads, card.MaxDeepReads = 2, 3
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1000, 320
		card.RequiredFields = []string{"版本", "前置條件", "操作步驟", "驗證方式", "已知限制"}
		card.SourcePriority = []string{"官方文件", "官方版本公告或原始碼", "可信技術說明"}
		card.Freshness = "步驟必須對應使用者版本；舊版本內容需標示"
		card.StopRule = "官方步驟與版本相符，且有可觀察的驗證方式"
		card.AnswerFormat = "前置條件、分步操作、驗證、限制與來源"
	case containsAnySearchMarker(lower,
		"比較", "比對", "差異", "差别", "優缺點", "哪个好", "哪個好", "推薦哪", " vs ", " versus ",
		"compare", "comparison", "difference", "pros and cons", "which is better"):
		card.Kind = SearchTaskComparison
		card.MinDeepReads, card.MaxDeepReads = 2, 4
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 1200, 320
		card.RequiredFields = []string{"共同準則", "差異", "適合對象", "改選條件", "限制"}
		card.SourcePriority = []string{"各方案第一手來源", "共同基準或獨立測試", "可信二手分析"}
		card.Freshness = "比較資料必須來自相容版本或相近日期"
		card.StopRule = "每個比較對象均有來源，且共同準則可直接對照"
		card.AnswerFormat = "同欄位比較表、結論、適合對象與改選條件"
	case containsAnySearchMarker(lower,
		"是什麼", "是什么", "解釋", "解释", "說明", "说明", "介紹", "介绍", "基本概念",
		"what is", "explain", "overview", "introduction"):
		card.Kind = SearchTaskKnowledgeExplanation
		card.MinDeepReads, card.MaxDeepReads = 2, 3
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 800, 512
		card.RequiredFields = []string{"直接定義", "核心原理", "適合受眾的例子", "來源"}
		card.SourcePriority = []string{"政府或教育機構", "學術或專業第一手來源", "可信教育內容"}
		card.Freshness = "穩定概念優先權威與教學清楚度；具時效性的細節仍保留更新日期"
		card.StopRule = "核心定義獲得兩個不同可靠來源支持，並以使用者程度可理解的方式回答"
		card.AnswerFormat = "直接解釋、簡單例子、來源；不要加入使用者未要求的比較或延伸表格"
	case containsAnySearchMarker(lower,
		"最新", "目前", "現在", "即時", "今天", "日期", "多少", "何時", "誰是", "政策", "數值",
		"latest", "current", "today", "when", "how much", "who is", "date", "policy", "current fact"):
		card.Kind = SearchTaskCurrentFact
		card.MinDeepReads, card.MaxDeepReads = 1, 2
		card.EvidenceTokenBudget, card.AnswerTokenBudget = 600, 128
		card.RequiredFields = []string{"答案值", "發布或更新日期", "適用範圍"}
		card.SourcePriority = []string{"官方或第一手來源", "可信獨立確認"}
		card.Freshness = "優先最新且有明確日期的來源"
		card.StopRule = "一個權威來源直接回答；有風險或衝突時增加第二來源"
		card.AnswerFormat = "直接答案、日期或適用範圍、來源與不確定性"
	}

	return card
}

// taskConstrainedSearchRAGCard narrows a broad method card only when the user
// explicitly asks for a subset of its fields. Method cards are defaults for
// ambiguous research requests, not a mandate to add version, tutorial, or
// recommendation sections that the user did not request.
func taskConstrainedSearchRAGCard(task string, card SearchRAGCard) SearchRAGCard {
	lower := strings.ToLower(strings.TrimSpace(task))
	if !containsAnySearchMarker(lower, "只", "僅", "仅", "only", "just", "項", "项") {
		return card
	}
	if explicit := explicitRequestedSearchFields(task); len(explicit) >= 2 {
		// A user-authored list is more authoritative than the broad method
		// card. For example, "三項前置條件：平台、Python、macOS" is not a
		// request for the software-guide defaults (version, tutorial steps,
		// verification, known limitations). Preserving the actual field names
		// keeps the evidence ledger from manufacturing irrelevant gaps and lets
		// the small research model spend its answer budget on the requested
		// facts. This is structural extraction only; no topic fact is encoded.
		card.RequiredFields = explicit
		card.AnswerFormat = "逐項直接回答使用者明列的欄位，最後附來源；不要增加未要求的通用章節"
		if card.AnswerTokenBudget < 320 {
			card.AnswerTokenBudget = 320
		}
		return card
	}
	requested := make([]string, 0, len(card.RequiredFields))
	for _, field := range card.RequiredFields {
		if strings.Contains(lower, strings.ToLower(field)) {
			requested = append(requested, field)
		}
	}
	if len(requested) == 0 || len(requested) == len(card.RequiredFields) {
		return card
	}
	card.RequiredFields = requested
	card.AnswerFormat = "直接回答使用者明確要求的欄位，附來源；不要增加未要求的通用章節"
	if card.AnswerTokenBudget > 240 {
		card.AnswerTokenBudget = 240
	}
	return card
}

func explicitRequestedSearchFields(task string) []string {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil
	}
	for offset := 0; offset < len(task); {
		relative := strings.IndexAny(task[offset:], "：:")
		if relative < 0 {
			break
		}
		colon := offset + relative
		prefixRunes := []rune(task[:colon])
		if len(prefixRunes) > 48 {
			prefixRunes = prefixRunes[len(prefixRunes)-48:]
		}
		prefix := strings.ToLower(string(prefixRunes))
		if strings.HasPrefix(task[colon:], "：") {
			offset = colon + len("：")
		} else {
			offset = colon + 1
		}
		if !containsAnySearchMarker(prefix,
			"項", "项", "欄位", "栏位", "字段", "條件", "条件",
			"資料", "资料", "資訊", "信息", "要點", "要点", "包含", "包括", "fields") {
			continue
		}
		end := len(task)
		if relativeEnd := strings.IndexAny(task[offset:], "。\n\r"); relativeEnd >= 0 {
			end = offset + relativeEnd
		}
		segment := strings.TrimSpace(task[offset:end])
		parts := strings.FieldsFunc(segment, func(r rune) bool {
			switch r {
			case '、', '，', ',', '；', ';':
				return true
			default:
				return false
			}
		})
		fields := make([]string, 0, len(parts))
		seen := make(map[string]bool)
		for _, part := range parts {
			part = strings.TrimSpace(strings.Trim(part, "-•*0123456789.()（）[]【】 "))
			if part == "" || len([]rune(part)) > 64 {
				continue
			}
			lowerPart := strings.ToLower(part)
			if containsAnySearchMarker(lowerPart,
				"請輸出", "请输出", "請使用", "请使用", "只使用", "僅使用", "透過", "通过", "來源", "source") {
				continue
			}
			if seen[lowerPart] {
				continue
			}
			seen[lowerPart] = true
			fields = append(fields, part)
		}
		if len(fields) >= 2 && len(fields) <= 8 {
			return fields
		}
	}
	return nil
}

// searchRAGRetrievalAliases adds compact cross-language field vocabulary to
// local semantic search. These are schema labels, not topic facts: they help a
// Chinese request retrieve an English heading such as "Requirements" without
// asking a 2.6B model to translate retrieval terminology on every task.
func searchRAGRetrievalAliases(card SearchRAGCard) []string {
	aliases := make([]string, 0, len(card.RequiredFields))
	seen := make(map[string]bool)
	for _, field := range card.RequiredFields {
		value := searchRAGFieldAlias(field)
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		aliases = append(aliases, value)
	}
	return aliases
}

// searchRAGFieldAlias translates only schema vocabulary, never topic facts or
// expected answers. It lets a Chinese field label retrieve and bind an English
// evidence passage while keeping all actual values source-derived.
func searchRAGFieldAlias(field string) string {
	lower := strings.ToLower(strings.TrimSpace(field))
	switch {
	case strings.Contains(lower, "處理器") || strings.Contains(lower, "处理器"):
		return "processor platform chip silicon"
	case strings.Contains(lower, "python") &&
		(strings.Contains(lower, "最低") || strings.Contains(lower, "minimum")):
		return "native python minimum version"
	case strings.Contains(lower, "macos") &&
		(strings.Contains(lower, "最低") || strings.Contains(lower, "minimum")):
		return "macos minimum version"
	case lower == "前置條件" || lower == "前置条件":
		return "requirements prerequisites must meet"
	case lower == "版本":
		return "version release"
	case lower == "操作步驟" || lower == "操作步骤":
		return "installation steps setup procedure"
	case lower == "驗證方式" || lower == "验证方式":
		return "verify verification check"
	case lower == "已知限制" || lower == "限制" || lower == "風險與限制" || lower == "风险与限制":
		return "limitations restrictions known issues"
	case lower == "發布或更新日期" || lower == "发布或更新日期" || lower == "日期" || lower == "庫存日期" || lower == "库存日期":
		return "published updated date"
	case lower == "官方售價" || lower == "官方售价" || lower == "通路價格" || lower == "通路价格" || lower == "價位" || lower == "价位" || lower == "費用" || lower == "费用":
		return "official price cost"
	case lower == "營業時間" || lower == "营业时间" || lower == "開放時間" || lower == "开放时间":
		return "opening hours business hours"
	case lower == "地址" || lower == "地點" || lower == "地点":
		return "address location"
	default:
		return ""
	}
}

func openResearchSearchRAGCard() SearchRAGCard {
	return SearchRAGCard{
		Kind: SearchTaskOpenResearch, CandidateLimit: 8,
		MinDeepReads: 2, MaxDeepReads: 4,
		EvidenceTokenBudget: 1200, AnswerTokenBudget: 320,
		RequiredFields: []string{"子問題", "已確認", "來源", "衝突", "缺口"},
		SourcePriority: []string{"第一手或官方來源", "原始研究或專業來源", "可信二手來源"},
		Freshness:      "依問題時效性保留發布或更新日期",
		StopRule:       "核心子問題有交叉證據，或剩餘缺口已明確披露",
		AnswerFormat:   "重點摘要、證據、衝突、缺口與來源",
	}
}

func containsAnySearchMarker(text string, markers ...string) bool {
	padded := " " + strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, text) + " "
	for _, marker := range markers {
		if strings.Contains(padded, marker) {
			return true
		}
	}
	return false
}
