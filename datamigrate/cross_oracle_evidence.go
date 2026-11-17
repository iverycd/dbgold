package datamigrate

import (
	"context"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"encoding/json"
)

type crossOracleEvidenceKey struct{}

func WithCrossOracleEvidence(ctx context.Context, seqs []source.SequenceInfo) context.Context {
	b, _ := json.Marshal(seqs)
	var copy []source.SequenceInfo
	_ = json.Unmarshal(b, &copy)
	return context.WithValue(ctx, crossOracleEvidenceKey{}, copy)
}
func CrossOracleDefinition(s source.SequenceInfo) string {
	if s.Native == nil {
		return ""
	}
	n := *s.Native
	n.OracleObjectID = 0
	s.Native = &n
	s.StartValue, s.StartValueText = 0, ""
	return repair.Hash(s)
}
func bindCrossOracleEvidence(ctx context.Context, s source.SequenceInfo) source.SequenceInfo {
	seqs, _ := ctx.Value(crossOracleEvidenceKey{}).([]source.SequenceInfo)
	for _, v := range seqs {
		if v.Native == nil || s.Native.Name != v.Native.Name || s.Native.Schema != v.Native.Schema {
			continue
		}
		if CrossOracleDefinition(s) != CrossOracleDefinition(v) {
			s.Unsupported = "序列源定义或绑定偏离持久化计划"
			continue
		}
		if s.Native.OracleObjectID > 0 && s.Native.OracleObjectID != v.Native.OracleObjectID {
			s.Unsupported = "序列持久化对象身份冲突"
			continue
		}
		s.Native.OracleObjectID = v.Native.OracleObjectID
	}
	return s
}

func CrossOracleEvidence(ctx context.Context) []source.SequenceInfo {
	seqs, _ := ctx.Value(crossOracleEvidenceKey{}).([]source.SequenceInfo)
	copy := WithCrossOracleEvidence(context.Background(), seqs)
	result, _ := copy.Value(crossOracleEvidenceKey{}).([]source.SequenceInfo)
	return result
}
