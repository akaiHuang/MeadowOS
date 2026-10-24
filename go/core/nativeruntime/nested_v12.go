package nativeruntime

import (
	"encoding/binary"
	"math"
	"sort"
	"unicode/utf8"
)

const (
	nestedMessageRole FieldID = 1
	nestedMessageContent FieldID = 2
	nestedMessageToolCalls FieldID = 3
	nestedMessageToolCallID FieldID = 4
	nestedPartKind FieldID = 1
	nestedPartText FieldID = 2
	nestedPartImageIndex FieldID = 3
	nestedPartImageDetail FieldID = 4
	nestedPartSourceURI FieldID = 5
	nestedToolType FieldID = 1
	nestedToolName FieldID = 2
	nestedToolDescription FieldID = 3
	nestedToolParameters FieldID = 4
	nestedCallID FieldID = 1
	nestedCallType FieldID = 2
	nestedCallName FieldID = 3
	nestedCallArguments FieldID = 4
	nestedChoiceMode FieldID = 1
	nestedChoiceFunction FieldID = 2
	nestedChoiceExtension FieldID = 3
	nestedEntryKey FieldID = 1
	nestedEntryValue FieldID = 2
	nestedValueKind FieldID = 1
	nestedValueScalar FieldID = 2
	nestedValueList FieldID = 3
	nestedValueMap FieldID = 4
)

func encodeRecordList(records [][]byte, limit int) ([]byte, error) {
	if len(records) > limit { return nil, protocolError(StatusResourceLimit, "nested record count exceeds %d", limit) }
	total := 4
	for _, record := range records {
		if uint64(len(record)) > MaxNestedRecordBytes { return nil, protocolError(StatusResourceLimit, "nested record exceeds %d bytes", MaxNestedRecordBytes) }
		total += 4 + len(record)
	}
	out := make([]byte, total)
	binary.LittleEndian.PutUint32(out[:4], uint32(len(records)))
	pos := 4
	for _, record := range records {
		binary.LittleEndian.PutUint32(out[pos:pos+4], uint32(len(record)))
		pos += 4
		copy(out[pos:], record)
		pos += len(record)
	}
	return out, nil
}

func decodeRecordList(data []byte, limit int) ([][]byte, error) {
	if len(data) < 4 { return nil, protocolError(StatusBadLength, "nested record list is truncated") }
	count := int(binary.LittleEndian.Uint32(data[:4]))
	if count > limit { return nil, protocolError(StatusResourceLimit, "nested record count exceeds %d", limit) }
	records := make([][]byte, 0, count)
	pos := 4
	for i := 0; i < count; i++ {
		if len(data)-pos < 4 { return nil, protocolError(StatusBadLength, "nested record %d length is truncated", i) }
		size := int(binary.LittleEndian.Uint32(data[pos:pos+4]))
		pos += 4
		if size > len(data)-pos || uint64(size) > MaxNestedRecordBytes { return nil, protocolError(StatusBadLength, "nested record %d is truncated", i) }
		records = append(records, append([]byte(nil), data[pos:pos+size]...))
		pos += size
	}
	if pos != len(data) { return nil, protocolError(StatusBadLength, "nested record list has trailing bytes") }
	return records, nil
}

func selectNested(parsed []wireField, rules map[FieldID]fieldRule) (map[FieldID]wireField, error) {
	known := make(map[FieldID]wireField, len(rules))
	for _, field := range parsed {
		rule, ok := rules[field.id]
		if !ok {
			if field.flags&FieldFlagCritical != 0 { return nil, protocolError(StatusUnknownCriticalField, "unknown critical nested field %d", field.id) }
			continue
		}
		if field.typ != rule.typ { return nil, protocolError(StatusBadEnum, "nested field %d has type %d, expected %d", field.id, field.typ, rule.typ) }
		known[field.id] = field
	}
	for id, rule := range rules { if rule.required { if _, ok := known[id]; !ok { return nil, protocolError(StatusMissingField, "missing nested field %d", id) } } }
	return known, nil
}

func encodeMessageList(messages []ChatMessage) ([]byte, error) {
	records := make([][]byte, 0, len(messages))
	for _, message := range messages {
		partRecords := make([][]byte, 0, len(message.Content))
		for _, part := range message.Content {
			fields := []wireField{requiredU32(nestedPartKind, uint32(part.Kind))}
			if part.Text != "" { fields = append(fields, wireField{id: nestedPartText, typ: WireUTF8, value: []byte(part.Text)}) }
			if part.ImageIndex != nil { fields = append(fields, optionalU32(nestedPartImageIndex, *part.ImageIndex)) }
			fields = appendOptionalString(fields, nestedPartImageDetail, part.ImageDetail)
			fields = appendOptionalString(fields, nestedPartSourceURI, part.SourceURI)
			record, err := encodeFields(fields); if err != nil { return nil, err }
			partRecords = append(partRecords, record)
		}
		parts, err := encodeRecordList(partRecords, MaxMessageParts); if err != nil { return nil, err }
		fields := []wireField{requiredU32(nestedMessageRole, uint32(message.Role)), requiredBytes(nestedMessageContent, parts)}
		if len(message.ToolCalls) > 0 { calls, err := encodeToolCallList(message.ToolCalls); if err != nil { return nil, err }; fields = append(fields, requiredBytes(nestedMessageToolCalls, calls)) }
		fields = appendOptionalString(fields, nestedMessageToolCallID, message.ToolCallID)
		record, err := encodeFields(fields); if err != nil { return nil, err }
		records = append(records, record)
	}
	return encodeRecordList(records, MaxMessages)
}

func decodeMessageList(data []byte) ([]ChatMessage, error) {
	records, err := decodeRecordList(data, MaxMessages); if err != nil { return nil, err }
	messages := make([]ChatMessage, 0, len(records))
	for _, record := range records {
		parsed, err := parseFields(record); if err != nil { return nil, err }
		fields, err := selectNested(parsed, map[FieldID]fieldRule{
			nestedMessageRole: {typ: WireU32, required: true}, nestedMessageContent: {typ: WireBytes, required: true}, nestedMessageToolCalls: {typ: WireBytes}, nestedMessageToolCallID: {typ: WireUTF8},
		}); if err != nil { return nil, err }
		message := ChatMessage{Role: MessageRole(getU32(fields, nestedMessageRole)), ToolCallID: optionalString(fields, nestedMessageToolCallID)}
		partRecords, err := decodeRecordList(fields[nestedMessageContent].value, MaxMessageParts); if err != nil { return nil, err }
		for _, partRecord := range partRecords {
			partParsed, err := parseFields(partRecord); if err != nil { return nil, err }
			partFields, err := selectNested(partParsed, map[FieldID]fieldRule{
				nestedPartKind: {typ: WireU32, required: true}, nestedPartText: {typ: WireUTF8}, nestedPartImageIndex: {typ: WireU32}, nestedPartImageDetail: {typ: WireUTF8}, nestedPartSourceURI: {typ: WireUTF8},
			}); if err != nil { return nil, err }
			part := MessageContentPart{Kind: ContentPartKind(getU32(partFields, nestedPartKind)), ImageIndex: optionalU32Value(partFields, nestedPartImageIndex), ImageDetail: optionalString(partFields, nestedPartImageDetail), SourceURI: optionalString(partFields, nestedPartSourceURI)}
			if value, ok := partFields[nestedPartText]; ok { part.Text = string(value.value) }
			message.Content = append(message.Content, part)
		}
		if value, ok := fields[nestedMessageToolCalls]; ok { message.ToolCalls, err = decodeToolCallList(value.value); if err != nil { return nil, err } }
		messages = append(messages, message)
	}
	return messages, nil
}

func encodeToolList(tools []ToolDefinition) ([]byte, error) {
	records := make([][]byte, 0, len(tools))
	for _, tool := range tools {
		record, err := encodeFields([]wireField{requiredUTF8(nestedToolType, tool.Type), requiredUTF8(nestedToolName, tool.Name), requiredUTF8(nestedToolDescription, tool.Description), requiredUTF8(nestedToolParameters, tool.ParametersJSON)})
		if err != nil { return nil, err }; records = append(records, record)
	}
	return encodeRecordList(records, MaxTools)
}

func decodeToolList(data []byte) ([]ToolDefinition, error) {
	records, err := decodeRecordList(data, MaxTools); if err != nil { return nil, err }
	tools := make([]ToolDefinition, 0, len(records))
	for _, record := range records {
		parsed, err := parseFields(record); if err != nil { return nil, err }
		fields, err := selectNested(parsed, map[FieldID]fieldRule{nestedToolType: {typ: WireUTF8, required: true}, nestedToolName: {typ: WireUTF8, required: true}, nestedToolDescription: {typ: WireUTF8, required: true}, nestedToolParameters: {typ: WireUTF8, required: true}}); if err != nil { return nil, err }
		tools = append(tools, ToolDefinition{Type: getString(fields, nestedToolType), Name: getString(fields, nestedToolName), Description: getString(fields, nestedToolDescription), ParametersJSON: getString(fields, nestedToolParameters)})
	}
	return tools, nil
}

func encodeToolCallList(calls []MessageToolCall) ([]byte, error) {
	records := make([][]byte, 0, len(calls))
	for _, call := range calls { record, err := encodeFields([]wireField{requiredUTF8(nestedCallID, call.ID), requiredUTF8(nestedCallType, call.Type), requiredUTF8(nestedCallName, call.Name), requiredUTF8(nestedCallArguments, call.Arguments)}); if err != nil { return nil, err }; records = append(records, record) }
	return encodeRecordList(records, MaxToolCalls)
}

func decodeToolCallList(data []byte) ([]MessageToolCall, error) {
	records, err := decodeRecordList(data, MaxToolCalls); if err != nil { return nil, err }
	calls := make([]MessageToolCall, 0, len(records))
	for _, record := range records { parsed, err := parseFields(record); if err != nil { return nil, err }; fields, err := selectNested(parsed, map[FieldID]fieldRule{nestedCallID: {typ: WireUTF8, required: true}, nestedCallType: {typ: WireUTF8, required: true}, nestedCallName: {typ: WireUTF8, required: true}, nestedCallArguments: {typ: WireUTF8, required: true}}); if err != nil { return nil, err }; calls = append(calls, MessageToolCall{ID: getString(fields, nestedCallID), Type: getString(fields, nestedCallType), Name: getString(fields, nestedCallName), Arguments: getString(fields, nestedCallArguments)}) }
	return calls, nil
}

func encodeToolChoice(choice ToolChoice) ([]byte, error) { fields := []wireField{requiredU32(nestedChoiceMode, uint32(choice.Mode))}; fields = appendOptionalString(fields, nestedChoiceFunction, choice.FunctionName); fields = appendOptionalString(fields, nestedChoiceExtension, choice.ExtensionJSON); return encodeFields(fields) }
func decodeToolChoice(data []byte) (*ToolChoice, error) { parsed, err := parseFields(data); if err != nil { return nil, err }; fields, err := selectNested(parsed, map[FieldID]fieldRule{nestedChoiceMode: {typ: WireU32, required: true}, nestedChoiceFunction: {typ: WireUTF8}, nestedChoiceExtension: {typ: WireUTF8}}); if err != nil { return nil, err }; return &ToolChoice{Mode: ToolChoiceMode(getU32(fields, nestedChoiceMode)), FunctionName: optionalString(fields, nestedChoiceFunction), ExtensionJSON: optionalString(fields, nestedChoiceExtension)}, nil }

func encodeStringList(values []string, limit int) ([]byte, error) { records := make([][]byte, len(values)); for i, value := range values { records[i] = []byte(value) }; return encodeRecordList(records, limit) }
func decodeStringList(data []byte, limit int) ([]string, error) { records, err := decodeRecordList(data, limit); if err != nil { return nil, err }; values := make([]string, len(records)); for i, record := range records { if !utf8.Valid(record) { return nil, protocolError(StatusBadUTF8, "string list item %d is not UTF-8", i) }; values[i] = string(record) }; return values, nil }

func encodeTypedMap(values map[string]TypedValue) ([]byte, error) { return encodeTypedMapDepth(values, 0) }
func encodeTypedMapDepth(values map[string]TypedValue, depth int) ([]byte, error) {
	if depth >= MaxTypedValueDepth { return nil, protocolError(StatusResourceLimit, "typed value nesting exceeds %d", MaxTypedValueDepth) }
	keys := make([]string, 0, len(values)); for key := range values { keys = append(keys, key) }; sort.Strings(keys)
	records := make([][]byte, 0, len(keys))
	for _, key := range keys { encoded, err := encodeTypedValueDepth(values[key], depth+1); if err != nil { return nil, err }; record, err := encodeFields([]wireField{requiredUTF8(nestedEntryKey, key), requiredBytes(nestedEntryValue, encoded)}); if err != nil { return nil, err }; records = append(records, record) }
	return encodeRecordList(records, MaxFields)
}

func decodeTypedMap(data []byte) (map[string]TypedValue, error) {
	records, err := decodeRecordList(data, MaxFields); if err != nil { return nil, err }; values := make(map[string]TypedValue, len(records))
	for _, record := range records { parsed, err := parseFields(record); if err != nil { return nil, err }; fields, err := selectNested(parsed, map[FieldID]fieldRule{nestedEntryKey: {typ: WireUTF8, required: true}, nestedEntryValue: {typ: WireBytes, required: true}}); if err != nil { return nil, err }; key := getString(fields, nestedEntryKey); if _, exists := values[key]; exists { return nil, protocolError(StatusDuplicateOrUnsortedField, "duplicate typed-map key %q", key) }; value, err := decodeTypedValue(fields[nestedEntryValue].value); if err != nil { return nil, err }; values[key] = value }
	return values, nil
}

func encodeTypedValue(value TypedValue) ([]byte, error) { return encodeTypedValueDepth(value, 0) }
func encodeTypedValueDepth(value TypedValue, depth int) ([]byte, error) {
	if depth >= MaxTypedValueDepth { return nil, protocolError(StatusResourceLimit, "typed value nesting exceeds %d", MaxTypedValueDepth) }
	fields := []wireField{requiredU32(nestedValueKind, uint32(value.Kind))}
	switch value.Kind {
	case TypedNull:
	case TypedBool: fields = append(fields, requiredBool(nestedValueScalar, value.Bool))
	case TypedI64: fields = append(fields, requiredU64(nestedValueScalar, uint64(value.I64)))
	case TypedU64: fields = append(fields, requiredU64(nestedValueScalar, value.U64))
	case TypedF64: fields = append(fields, requiredF64(nestedValueScalar, value.F64))
	case TypedUTF8: fields = append(fields, requiredUTF8(nestedValueScalar, value.UTF8))
	case TypedBytes: fields = append(fields, requiredBytes(nestedValueScalar, value.Bytes))
	case TypedList: records := make([][]byte, 0, len(value.List)); for _, item := range value.List { encoded, err := encodeTypedValueDepth(item, depth+1); if err != nil { return nil, err }; records = append(records, encoded) }; list, err := encodeRecordList(records, MaxFields); if err != nil { return nil, err }; fields = append(fields, requiredBytes(nestedValueList, list))
	case TypedMap: encoded, err := encodeTypedMapDepth(value.Map, depth+1); if err != nil { return nil, err }; fields = append(fields, requiredBytes(nestedValueMap, encoded))
	default: return nil, protocolError(StatusBadEnum, "invalid typed value kind %d", value.Kind)
	}
	return encodeFields(fields)
}

func decodeTypedValue(data []byte) (TypedValue, error) { return decodeTypedValueDepth(data,0) }
func decodeTypedValueDepth(data []byte,depth int) (TypedValue, error) {
	if depth>=MaxTypedValueDepth{return TypedValue{},protocolError(StatusResourceLimit,"typed value nesting exceeds %d",MaxTypedValueDepth)}
	parsed, err := parseFields(data); if err != nil { return TypedValue{}, err }
	fields:=make(map[FieldID]wireField,len(parsed));for _,field:=range parsed{switch field.id{case nestedValueKind:if field.typ!=WireU32{return TypedValue{},protocolError(StatusBadEnum,"typed value kind has invalid wire type")};case nestedValueScalar:case nestedValueList,nestedValueMap:if field.typ!=WireBytes{return TypedValue{},protocolError(StatusBadEnum,"typed container has invalid wire type")};default:if field.flags&FieldFlagCritical!=0{return TypedValue{},protocolError(StatusUnknownCriticalField,"unknown critical typed-value field %d",field.id)};continue};fields[field.id]=field}
	kindField, ok := fields[nestedValueKind]; if !ok { return TypedValue{}, protocolError(StatusMissingField, "typed value kind is required") }
	value := TypedValue{Kind: TypedValueKind(binary.LittleEndian.Uint32(kindField.value))}
	scalar, hasScalar := fields[nestedValueScalar];_,hasList:=fields[nestedValueList];_,hasMap:=fields[nestedValueMap]
	switch value.Kind {
	case TypedNull: if hasScalar||hasList||hasMap { return TypedValue{}, protocolError(StatusInvalidState, "null typed value cannot carry data") }
	case TypedBool: if !hasScalar || scalar.typ != WireBool||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed bool has invalid wire type") }; value.Bool = scalar.value[0] == 1
	case TypedI64: if !hasScalar || scalar.typ != WireU64||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed i64 has invalid wire type") }; value.I64 = int64(binary.LittleEndian.Uint64(scalar.value))
	case TypedU64: if !hasScalar || scalar.typ != WireU64||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed u64 has invalid wire type") }; value.U64 = binary.LittleEndian.Uint64(scalar.value)
	case TypedF64: if !hasScalar || scalar.typ != WireF64||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed f64 has invalid wire type") }; value.F64 = math.Float64frombits(binary.LittleEndian.Uint64(scalar.value));if math.IsNaN(value.F64)||math.IsInf(value.F64,0){return TypedValue{},protocolError(StatusBadEnum,"typed f64 must be finite")}
	case TypedUTF8: if !hasScalar || scalar.typ != WireUTF8||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed string has invalid wire type") }; value.UTF8 = string(scalar.value)
	case TypedBytes: if !hasScalar || scalar.typ != WireBytes||hasList||hasMap { return TypedValue{}, protocolError(StatusBadEnum, "typed bytes has invalid wire type") }; value.Bytes = append([]byte(nil), scalar.value...)
	case TypedList: if hasScalar||hasMap{return TypedValue{},protocolError(StatusInvalidState,"typed list carries conflicting data")};listField, ok := fields[nestedValueList]; if !ok { return TypedValue{}, protocolError(StatusMissingField, "typed list payload is required") }; records, err := decodeRecordList(listField.value, MaxFields); if err != nil { return TypedValue{}, err }; for _, record := range records { item, err := decodeTypedValueDepth(record,depth+1); if err != nil { return TypedValue{}, err }; value.List = append(value.List, item) }
	case TypedMap: if hasScalar||hasList{return TypedValue{},protocolError(StatusInvalidState,"typed map carries conflicting data")};mapField, ok := fields[nestedValueMap]; if !ok { return TypedValue{}, protocolError(StatusMissingField, "typed map payload is required") }; value.Map, err = decodeTypedMapDepth(mapField.value,depth+1); if err != nil { return TypedValue{}, err }
	default: return TypedValue{}, protocolError(StatusBadEnum, "invalid typed value kind %d", value.Kind)
	}
	return value, nil
}

func decodeTypedMapDepth(data []byte,depth int)(map[string]TypedValue,error){if depth>=MaxTypedValueDepth{return nil,protocolError(StatusResourceLimit,"typed map nesting exceeds %d",MaxTypedValueDepth)};records,err:=decodeRecordList(data,MaxFields);if err!=nil{return nil,err};values:=make(map[string]TypedValue,len(records));for _,record:=range records{parsed,err:=parseFields(record);if err!=nil{return nil,err};fields,err:=selectNested(parsed,map[FieldID]fieldRule{nestedEntryKey:{typ:WireUTF8,required:true},nestedEntryValue:{typ:WireBytes,required:true}});if err!=nil{return nil,err};key:=getString(fields,nestedEntryKey);if _,exists:=values[key];exists{return nil,protocolError(StatusDuplicateOrUnsortedField,"duplicate typed-map key %q",key)};value,err:=decodeTypedValueDepth(fields[nestedEntryValue].value,depth);if err!=nil{return nil,err};values[key]=value};return values,nil}
