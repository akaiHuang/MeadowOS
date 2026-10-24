package attachments

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	DocumentMaxBytes         = 20 << 20
	DocumentMaxTextChars     = 256_000
	documentMaxXMLBytes      = 16 << 20
	documentMaxExpandedBytes = 128 << 20
	documentMaxZIPEntries    = 2048
	documentMaxUnits         = 256
)

type DocumentFormat struct{ Extension, MediaType string }

var documentFormats = map[string]string{
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".csv":  "text/csv", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".txt":  "text/plain", ".md": "text/markdown",
}

// DocumentType accepts a filename and a compatible canonical media type. File
// semantics remain opaque: this is format validation, never task routing.
func DocumentType(name, mediaType string) (DocumentFormat, error) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".xls" {
		return DocumentFormat{}, fmt.Errorf("舊版 XLS 尚未支援，請轉存 XLSX 或 CSV 後附加")
	}
	want, ok := documentFormats[ext]
	if !ok {
		return DocumentFormat{}, fmt.Errorf("文件附件支援 XLSX、CSV、DOCX、PPTX、TXT、MD")
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	if mediaType != "" && mediaType != want && mediaType != "application/octet-stream" &&
		!(ext == ".md" && mediaType == "text/plain") {
		return DocumentFormat{}, fmt.Errorf("文件副檔名與內容類型不符")
	}
	return DocumentFormat{Extension: ext, MediaType: want}, nil
}

type DocumentExtract struct {
	Text       string
	Truncated  bool
	Units      int
	LimitChars int
	Note       string
}

type documentText struct {
	strings.Builder
	chars     int
	truncated bool
}

func (b *documentText) add(text string) {
	remaining := DocumentMaxTextChars - b.chars
	if remaining <= 0 {
		if text != "" {
			b.truncated = true
		}
		return
	}
	runes := []rune(text)
	if len(runes) > remaining {
		runes = runes[:remaining]
		b.truncated = true
	}
	b.WriteString(string(runes))
	b.chars += len(runes)
}

// ExtractDocument only interprets bounded container/text structure. It never
// evaluates formulas, executes embedded code, follows links or runs processes.
func ExtractDocument(name, mediaType string, data []byte) (DocumentExtract, error) {
	format, err := DocumentType(name, mediaType)
	if err != nil {
		return DocumentExtract{}, err
	}
	if len(data) == 0 || len(data) > DocumentMaxBytes {
		return DocumentExtract{}, fmt.Errorf("文件必須大於 0 且不超過 20 MB")
	}
	b := &documentText{}
	result := DocumentExtract{LimitChars: DocumentMaxTextChars}
	switch format.Extension {
	case ".txt", ".md", ".csv":
		data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
		if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
			return result, fmt.Errorf("文字文件必須是有效的 UTF-8，不能包含二進位內容")
		}
		for _, r := range string(data) {
			if r < 32 && r != '\n' && r != '\r' && r != '\t' {
				return result, fmt.Errorf("文字文件包含不支援的控制字元")
			}
		}
		if format.Extension == ".csv" {
			reader := csv.NewReader(bytes.NewReader(data))
			reader.FieldsPerRecord = -1
			for row := 1; ; row++ {
				fields, readErr := reader.Read()
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return result, fmt.Errorf("CSV 第 %d 列格式無效：%w", row, readErr)
				}
				b.add(fmt.Sprintf("Row %d:", row))
				for col, value := range fields {
					b.add(fmt.Sprintf(" %s=%q", columnName(col+1), value))
				}
				b.add("\n")
				result.Units = row
			}
			result.Note = "CSV 依原始列與欄位順序；所有內容視為資料，不執行公式。"
		} else {
			b.add(string(data))
			result.Units = 1
			result.Note = "UTF-8 原文；內容視為來源資料。"
		}
	default:
		archive, openErr := openDocumentZIP(data, format.Extension)
		if openErr != nil {
			return result, openErr
		}
		switch format.Extension {
		case ".xlsx":
			result.Units, err = extractWorkbook(archive, b)
			result.Note = "XLSX 儲存格原始值；公式只列出檔案內運算式與快取值，不重新計算。日期、數值格式未套用；圖表、圖片、註解不含於文字。"
		case ".docx":
			var body []byte
			body, err = archive.read(archive.mainPart)
			if err == nil {
				err = extractXMLText(body, "document", b)
				result.Units = 1
			}
			result.Note = "DOCX 主文段落與表格文字，依 XML 順序；AlternateContent 選擇支援的基本文字命名空間 Choice，未知擴充採 Fallback，不重複兩分支。未抽取頁首頁尾、圖片、註解或視覺版面。"
		case ".pptx":
			result.Units, err = extractPresentation(archive, b)
			result.Note = "PPTX 依簡報投影片順序抽取文字與表格；AlternateContent 選擇支援的基本文字命名空間 Choice，未知擴充採 Fallback。未抽取講者備註、圖片、圖表數據或視覺版面。"
		}
		if err != nil {
			return result, err
		}
	}
	result.Text = b.String()
	result.Truncated = b.truncated
	if strings.TrimSpace(result.Text) == "" {
		return result, fmt.Errorf("文件沒有可抽取的文字；圖片或掃描內容請改用圖片或 PDF")
	}
	return result, nil
}

type documentZIP struct {
	files    map[string]*zip.File
	mainPart string
}

func openDocumentZIP(data []byte, extension string) (*documentZIP, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("Office 文件不是有效 ZIP 容器")
	}
	if len(reader.File) > documentMaxZIPEntries {
		return nil, fmt.Errorf("Office 文件項目超過 %d", documentMaxZIPEntries)
	}
	result := &documentZIP{files: map[string]*zip.File{}}
	var expanded uint64
	for _, file := range reader.File {
		name := file.Name
		if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != strings.TrimSuffix(name, "/") || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("Office 文件包含不安全的項目路徑")
		}
		if file.Flags&1 != 0 {
			return nil, fmt.Errorf("不支援加密 Office 文件")
		}
		if _, exists := result.files[name]; exists {
			return nil, fmt.Errorf("Office 文件包含重複項目")
		}
		if strings.Contains(strings.ToLower(name), "vbaproject") {
			return nil, fmt.Errorf("不支援含巨集的 Office 文件")
		}
		if file.UncompressedSize64 > documentMaxExpandedBytes-expanded {
			return nil, fmt.Errorf("Office 文件解壓後超過 128 MB")
		}
		expanded += file.UncompressedSize64
		result.files[name] = file
	}
	if _, ok := result.files["[Content_Types].xml"]; !ok {
		return nil, fmt.Errorf("Office 文件缺少內容類型資訊")
	}
	manifest, err := result.read("[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	var types struct {
		XMLName  xml.Name `xml:"Types"`
		Defaults []struct {
			Extension string `xml:"Extension,attr"`
			Type      string `xml:"ContentType,attr"`
		} `xml:"Default"`
		Overrides []struct {
			Part string `xml:"PartName,attr"`
			Type string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	if err := unmarshalOfficeXML(manifest, &types); err != nil {
		return nil, err
	}
	mainTypes := map[string]string{
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml",
	}
	rootData, err := result.read("_rels/.rels")
	if err != nil {
		return nil, err
	}
	var rootRels struct {
		XMLName xml.Name             `xml:"Relationships"`
		Items   []officeRelationship `xml:"Relationship"`
	}
	if err := unmarshalOfficeXML(rootData, &rootRels); err != nil {
		return nil, err
	}
	for _, rel := range rootRels.Items {
		if rel.Type != "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" && rel.Type != "http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument" {
			continue
		}
		if strings.EqualFold(rel.Mode, "External") || result.mainPart != "" {
			return nil, fmt.Errorf("Office 文件主內容關聯無效")
		}
		result.mainPart, err = documentPartTarget("", rel.Target)
		if err != nil {
			return nil, err
		}
	}
	if result.mainPart == "" {
		return nil, fmt.Errorf("Office 文件缺少主內容關聯")
	}
	defaults := make(map[string]string, len(types.Defaults))
	for _, item := range types.Defaults {
		if strings.Contains(strings.ToLower(item.Type), "macroenabled") {
			return nil, fmt.Errorf("不支援含巨集的 Office 文件")
		}
		ext := strings.ToLower(item.Extension)
		if ext == "" || strings.ContainsAny(ext, ". /\\\x00") || item.Type == "" {
			return nil, fmt.Errorf("Office 文件預設內容類型無效")
		}
		if _, exists := defaults[ext]; exists {
			return nil, fmt.Errorf("Office 文件包含重複內容類型")
		}
		defaults[ext] = item.Type
	}
	overrides := make(map[string]string, len(types.Overrides))
	for _, item := range types.Overrides {
		if strings.Contains(strings.ToLower(item.Type), "macroenabled") {
			return nil, fmt.Errorf("不支援含巨集的 Office 文件")
		}
		if _, exists := overrides[item.Part]; exists {
			return nil, fmt.Errorf("Office 文件包含重複內容類型")
		}
		overrides[item.Part] = item.Type
	}
	// OPC resolves a part-specific Override before its extension Default.
	mainType, overridden := overrides["/"+result.mainPart]
	if !overridden {
		mainType = defaults[strings.ToLower(strings.TrimPrefix(path.Ext(result.mainPart), "."))]
	}
	if mainType != mainTypes[extension] {
		return nil, fmt.Errorf("Office 文件主內容類型與副檔名不符")
	}
	return result, nil
}
func (z *documentZIP) read(name string) ([]byte, error) {
	file, ok := z.files[name]
	if !ok {
		return nil, fmt.Errorf("Office 文件缺少 %s", name)
	}
	if file.UncompressedSize64 > documentMaxXMLBytes {
		return nil, fmt.Errorf("Office XML 項目超過 16 MB")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("Office 項目無法讀取")
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, documentMaxXMLBytes+1))
	if err != nil || len(data) > documentMaxXMLBytes {
		return nil, fmt.Errorf("Office XML 項目無效或超過 16 MB")
	}
	return data, nil
}

func extractXMLText(data []byte, root string, out *documentText) error {
	var valid struct{ XMLName xml.Name }
	if err := unmarshalOfficeXML(data, &valid); err != nil {
		return err
	}
	if valid.XMLName.Local != root {
		return fmt.Errorf("Office XML 根元素不符")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	first := true
	depth, capture := 0, 0
	type alternateBranch struct {
		depth    int
		selected bool
	}
	var alternates []alternateBranch
	var namespaceScopes []map[string]string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if depth != 0 || first {
				return fmt.Errorf("Office XML 內容不完整")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("Office XML 格式無效：%w", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			return fmt.Errorf("Office XML 不支援 DTD 指令")
		case xml.StartElement:
			if first {
				if token.Name.Local != root {
					return fmt.Errorf("Office XML 根元素不符")
				}
				first = false
			}
			depth++
			scope := map[string]string{}
			if len(namespaceScopes) > 0 {
				for prefix, namespace := range namespaceScopes[len(namespaceScopes)-1] {
					scope[prefix] = namespace
				}
			}
			for _, attr := range token.Attr {
				if attr.Name.Space == "xmlns" {
					scope[attr.Name.Local] = attr.Value
				}
			}
			namespaceScopes = append(namespaceScopes, scope)
			if token.Name.Space == "http://schemas.openxmlformats.org/markup-compatibility/2006" {
				if token.Name.Local == "AlternateContent" {
					alternates = append(alternates, alternateBranch{depth: depth})
				}
				if (token.Name.Local == "Choice" || token.Name.Local == "Fallback") && len(alternates) > 0 && depth == alternates[len(alternates)-1].depth+1 {
					branch := &alternates[len(alternates)-1]
					if branch.selected || (token.Name.Local == "Choice" && !supportedOfficeChoice(token.Attr, scope)) {
						if err := decoder.Skip(); err != nil {
							return fmt.Errorf("Office AlternateContent 格式無效：%w", err)
						}
						depth--
						namespaceScopes = namespaceScopes[:len(namespaceScopes)-1]
						continue
					}
					branch.selected = true
				}
			}
			if depth > 128 {
				return fmt.Errorf("Office XML 巢狀深度超過限制")
			}
			if token.Name.Local == "t" {
				capture++
			}
			if token.Name.Local == "tab" {
				out.add("\t")
			}
			if token.Name.Local == "br" || token.Name.Local == "cr" {
				out.add("\n")
			}
		case xml.CharData:
			if capture > 0 {
				out.add(string(token))
			}
		case xml.EndElement:
			if token.Name.Local == "AlternateContent" && token.Name.Space == "http://schemas.openxmlformats.org/markup-compatibility/2006" && len(alternates) > 0 {
				alternates = alternates[:len(alternates)-1]
			}
			if token.Name.Local == "t" {
				capture--
			}
			if token.Name.Local == "p" || token.Name.Local == "tr" {
				out.add("\n")
			}
			if token.Name.Local == "tc" {
				out.add("\t")
			}
			depth--
			namespaceScopes = namespaceScopes[:len(namespaceScopes)-1]
		}
	}
}

// A text extractor understands the base Office text vocabularies. Unknown
// extension choices fall back rather than combining exclusive alternatives.
func supportedOfficeChoice(attrs []xml.Attr, namespaces map[string]string) bool {
	for _, attr := range attrs {
		if attr.Name.Local != "Requires" || attr.Name.Space != "" {
			continue
		}
		prefixes := strings.Fields(attr.Value)
		if len(prefixes) == 0 {
			return false
		}
		for _, prefix := range prefixes {
			namespace := namespaces[prefix]
			supported := false
			for _, family := range []string{"wordprocessingml", "presentationml", "drawingml"} {
				supported = supported || namespace == "http://schemas.openxmlformats.org/"+family+"/2006/main" || namespace == "http://purl.oclc.org/ooxml/"+family+"/main"
			}
			if !supported {
				return false
			}
		}
		return true
	}
	return false
}

type officeRelationship struct {
	ID     string `xml:"Id,attr"`
	Target string `xml:"Target,attr"`
	Mode   string `xml:"TargetMode,attr"`
	Type   string `xml:"Type,attr"`
}

func documentPartTarget(parent, target string) (string, error) {
	if target == "" || strings.ContainsAny(target, "\\\x00:?#") {
		return "", fmt.Errorf("Office 文件關聯路徑無效")
	}
	clean := path.Clean(path.Join(parent, target))
	if strings.HasPrefix(target, "/") {
		clean = path.Clean(strings.TrimPrefix(target, "/"))
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return "", fmt.Errorf("Office 文件關聯路徑無效")
	}
	return clean, nil
}

func documentRelationshipsPath(part string) string {
	return path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
}

func officeRelationships(z *documentZIP, name, parent string, kinds ...string) (map[string]string, error) {
	data, err := z.read(name)
	if err != nil {
		return nil, err
	}
	var doc struct {
		XMLName xml.Name             `xml:"Relationships"`
		Items   []officeRelationship `xml:"Relationship"`
	}
	if err := unmarshalOfficeXML(data, &doc); err != nil {
		return nil, err
	}
	items := map[string]string{}
	seen := map[string]bool{}
	for _, r := range doc.Items {
		if r.ID == "" || seen[r.ID] {
			return nil, fmt.Errorf("Office 文件關聯 ID 無效或重複")
		}
		seen[r.ID] = true
		accepted := len(kinds) == 0
		for _, kind := range kinds {
			accepted = accepted || officeRelationshipType(r.Type, kind)
		}
		if !accepted || strings.EqualFold(r.Mode, "External") {
			continue
		}
		target, err := documentPartTarget(parent, r.Target)
		if err != nil {
			return nil, err
		}
		items[r.ID] = target
	}
	return items, nil
}

func officeRelationshipType(value, kind string) bool {
	return value == "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"+kind || value == "http://purl.oclc.org/ooxml/officeDocument/relationships/"+kind
}

func officeRelationshipID(attrs []xml.Attr) string {
	var id string
	for _, attr := range attrs {
		if attr.Name.Local == "id" && (attr.Name.Space == "http://schemas.openxmlformats.org/officeDocument/2006/relationships" || attr.Name.Space == "http://purl.oclc.org/ooxml/officeDocument/relationships") {
			if id != "" {
				return ""
			}
			id = attr.Value
		}
	}
	return id
}
func unmarshalOfficeXML(data []byte, target any) error {
	// A single leading UTF-8 BOM is an encoding signature, not XML content.
	// Do not remove interior markers or tolerate arbitrary outside-root text.
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	// Reject directives and excessive nesting before DecodeElement can allocate.
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	roots := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("Office XML 格式無效：%w", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			return fmt.Errorf("Office XML 不支援 DTD 指令")
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || !officeRootNamespace(token.Name) {
					return fmt.Errorf("Office XML 根元素或命名空間無效")
				}
			}
			depth++
			if depth > 128 {
				return fmt.Errorf("Office XML 巢狀深度超過限制")
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return fmt.Errorf("Office XML 根元素之外有無效內容")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return fmt.Errorf("Office XML 內容不完整")
	}
	if err := xml.Unmarshal(data, target); err != nil {
		return fmt.Errorf("Office XML 格式無效：%w", err)
	}
	return nil
}

func officeRootNamespace(name xml.Name) bool {
	var family string
	switch name.Local {
	case "Types":
		return name.Space == "http://schemas.openxmlformats.org/package/2006/content-types"
	case "Relationships":
		return name.Space == "http://schemas.openxmlformats.org/package/2006/relationships"
	case "workbook", "worksheet", "sst", "chartsheet":
		family = "spreadsheetml"
	case "document":
		family = "wordprocessingml"
	case "presentation", "sld":
		family = "presentationml"
	default:
		return false
	}
	return name.Space == "http://schemas.openxmlformats.org/"+family+"/2006/main" || name.Space == "http://purl.oclc.org/ooxml/"+family+"/main"
}
func extractWorkbook(z *documentZIP, out *documentText) (int, error) {
	data, err := z.read(z.mainPart)
	if err != nil {
		return 0, err
	}
	var workbook struct {
		XMLName xml.Name `xml:"workbook"`
		Sheets  []struct {
			Name  string     `xml:"name,attr"`
			Attrs []xml.Attr `xml:",any,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := unmarshalOfficeXML(data, &workbook); err != nil {
		return 0, err
	}
	if len(workbook.Sheets) == 0 || len(workbook.Sheets) > documentMaxUnits {
		return 0, fmt.Errorf("XLSX 工作表數量必須為 1 到 %d", documentMaxUnits)
	}
	relsPath, relsParent := documentRelationshipsPath(z.mainPart), path.Dir(z.mainPart)
	rels, err := officeRelationships(z, relsPath, relsParent, "worksheet", "chartsheet")
	if err != nil {
		return 0, err
	}
	var shared []string
	sharedPath := ""
	if relData, err := z.read(relsPath); err == nil {
		var typed struct {
			XMLName xml.Name             `xml:"Relationships"`
			Items   []officeRelationship `xml:"Relationship"`
		}
		if err := unmarshalOfficeXML(relData, &typed); err != nil {
			return 0, err
		}
		for _, rel := range typed.Items {
			if officeRelationshipType(rel.Type, "sharedStrings") && !strings.EqualFold(rel.Mode, "External") {
				sharedPath, err = documentPartTarget(relsParent, rel.Target)
				if err != nil {
					return 0, err
				}
			}
		}
	}
	// Conventional-table compatibility for minimal producers that omit the
	// shared-string relationship. Custom tables use their actual relationship.
	if sharedPath == "" {
		if _, exists := z.files["xl/sharedStrings.xml"]; exists {
			sharedPath = "xl/sharedStrings.xml"
		}
	}
	if sharedPath != "" {
		data, err := z.read(sharedPath)
		if err != nil {
			return 0, err
		}
		var valid struct {
			XMLName xml.Name `xml:"sst"`
		}
		if err := unmarshalOfficeXML(data, &valid); err != nil {
			return 0, err
		}
		decoder := xml.NewDecoder(bytes.NewReader(data))
		var current strings.Builder
		inItem, inText := false, false
		phoneticDepth := 0
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, fmt.Errorf("XLSX sharedStrings XML 無效")
			}
			switch t := token.(type) {
			case xml.Directive:
				return 0, fmt.Errorf("Office XML 不支援 DTD 指令")
			case xml.StartElement:
				if t.Name.Local == "rPh" {
					phoneticDepth++
				}
				if t.Name.Local == "si" {
					inItem = true
					current.Reset()
				}
				if t.Name.Local == "t" {
					inText = true
				}
			case xml.CharData:
				if inItem && inText && phoneticDepth == 0 {
					current.Write(t)
				}
			case xml.EndElement:
				if t.Name.Local == "rPh" {
					phoneticDepth--
				}
				if t.Name.Local == "t" {
					inText = false
				}
				if t.Name.Local == "si" {
					shared = append(shared, decodeSpreadsheetString(current.String()))
					inItem = false
				}
			}
		}
	}
	for number, sheet := range workbook.Sheets {
		target, ok := rels[officeRelationshipID(sheet.Attrs)]
		if !ok {
			return 0, fmt.Errorf("XLSX 工作表關聯遺失")
		}
		data, err := z.read(target)
		if err != nil {
			return 0, err
		}
		var kind struct{ XMLName xml.Name }
		if err := unmarshalOfficeXML(data, &kind); err != nil {
			return 0, err
		}
		if kind.XMLName.Local == "chartsheet" {
			out.add(fmt.Sprintf("Sheet %d: %q\n[Chart sheet: no worksheet cell table; chart text/data omitted.]\n", number+1, decodeSpreadsheetString(sheet.Name)))
			continue
		}
		var worksheet struct {
			XMLName xml.Name `xml:"worksheet"`
			Rows    []struct {
				Number string `xml:"r,attr"`
				Cells  []struct {
					Ref     string  `xml:"r,attr"`
					Type    string  `xml:"t,attr"`
					Value   *string `xml:"v"`
					Formula *struct {
						Text   string `xml:",chardata"`
						Type   string `xml:"t,attr"`
						Shared string `xml:"si,attr"`
					} `xml:"f"`
					Inline struct {
						Text string `xml:"t"`
						Runs []struct {
							Text string `xml:"t"`
						} `xml:"r"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err := unmarshalOfficeXML(data, &worksheet); err != nil {
			return 0, err
		}
		out.add(fmt.Sprintf("Sheet %d: %q\n", number+1, decodeSpreadsheetString(sheet.Name)))
		for _, row := range worksheet.Rows {
			out.add("Row " + row.Number + ":")
			for _, cell := range row.Cells {
				value := ""
				if cell.Value != nil {
					value = *cell.Value
				}
				switch cell.Type {
				case "s":
					index, err := strconv.Atoi(value)
					if err != nil || index < 0 || index >= len(shared) {
						return 0, fmt.Errorf("XLSX shared string 索引無效")
					}
					value = shared[index]
				case "inlineStr":
					value = cell.Inline.Text
					for _, run := range cell.Inline.Runs {
						value += run.Text
					}
					value = decodeSpreadsheetString(value)
				case "str", "e":
					value = decodeSpreadsheetString(value)
				case "b":
					if value == "1" {
						value = "true"
					} else if value == "0" {
						value = "false"
					}
				}
				out.add(fmt.Sprintf(" %s=%q", cell.Ref, value))
				if cell.Formula != nil {
					expression := cell.Formula.Text
					if expression == "" {
						expression = "[shared formula definition not repeated; si=" + cell.Formula.Shared + "]"
					}
					if cell.Value == nil {
						out.add(fmt.Sprintf(" [formula=%q; cached=unavailable; not evaluated]", expression))
					} else {
						out.add(fmt.Sprintf(" [formula=%q; cached=%q; not evaluated]", expression, value))
					}
				}
			}
			out.add("\n")
		}
	}
	return len(workbook.Sheets), nil
}
func extractPresentation(z *documentZIP, out *documentText) (int, error) {
	data, err := z.read(z.mainPart)
	if err != nil {
		return 0, err
	}
	var doc struct {
		XMLName xml.Name `xml:"presentation"`
		Slides  []struct {
			ID string `xml:"id,attr"`
		} `xml:"sldIdLst>sldId"`
	}
	var valid struct {
		XMLName xml.Name `xml:"presentation"`
	}
	if err := unmarshalOfficeXML(data, &valid); err != nil {
		return 0, err
	}
	// sldId has both numeric id and relationship r:id; select namespaced r:id.
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("PPTX presentation XML 無效")
		}
		if t, ok := token.(xml.StartElement); ok && t.Name.Local == "sldId" {
			if id := officeRelationshipID(t.Attr); id != "" {
				doc.Slides = append(doc.Slides, struct {
					ID string `xml:"id,attr"`
				}{id})
			}
		}
	}
	if len(doc.Slides) == 0 || len(doc.Slides) > documentMaxUnits {
		return 0, fmt.Errorf("PPTX 投影片數量必須為 1 到 %d", documentMaxUnits)
	}
	rels, err := officeRelationships(z, documentRelationshipsPath(z.mainPart), path.Dir(z.mainPart), "slide")
	if err != nil {
		return 0, err
	}
	for i, slide := range doc.Slides {
		target, ok := rels[slide.ID]
		if !ok {
			return 0, fmt.Errorf("PPTX 投影片關聯遺失")
		}
		data, err := z.read(target)
		if err != nil {
			return 0, err
		}
		out.add(fmt.Sprintf("Slide %d:\n", i+1))
		if err := extractXMLText(data, "sld", out); err != nil {
			return 0, err
		}
		out.add("\n")
	}
	return len(doc.Slides), nil
}
func decodeSpreadsheetString(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		code, ok := spreadsheetEscapeAt(value, i)
		if !ok {
			out.WriteByte(value[i])
			i++
			continue
		}
		if code >= 0xd800 && code <= 0xdbff {
			if low, ok := spreadsheetEscapeAt(value, i+7); ok && low >= 0xdc00 && low <= 0xdfff {
				out.WriteRune(utf16.DecodeRune(rune(code), rune(low)))
				i += 14
				continue
			}
			out.WriteString(value[i : i+7])
			i += 7
			continue
		}
		if code >= 0xdc00 && code <= 0xdfff {
			out.WriteString(value[i : i+7])
		} else {
			out.WriteRune(rune(code))
		}
		i += 7
	}
	return out.String()
}

func spreadsheetEscapeAt(value string, i int) (uint64, bool) {
	if i < 0 || i+7 > len(value) || value[i:i+2] != "_x" || value[i+6] != '_' {
		return 0, false
	}
	code, err := strconv.ParseUint(value[i+2:i+6], 16, 16)
	return code, err == nil
}

func columnName(n int) string {
	var result string
	for n > 0 {
		n--
		result = string(rune('A'+n%26)) + result
		n /= 26
	}
	return result
}
