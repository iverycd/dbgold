package target

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuiltinPinyinPinnedAsset(t *testing.T) {
	const expectedHash = "7095903cd3bb7239e9b1c9394a5b5a8a76d3e4c08d77008d292d720ecd7ad103"
	require.Len(t, builtinPinyinInitials, 192394)
	sum := sha256.Sum256([]byte(builtinPinyinInitials))
	require.Equal(t, expectedHash, hex.EncodeToString(sum[:]))
	mapped := 0
	letters := map[byte]bool{}
	for i := 0; i < len(builtinPinyinInitials); i++ {
		c := builtinPinyinInitials[i]
		if c != '?' {
			require.True(t, c >= 'a' && c <= 'z')
			mapped++
			letters[c] = true
		}
	}
	require.Equal(t, 44348, mapped)
	require.Len(t, letters, 23)
	for _, c := range []byte("abcdefghjklmnopqrstwxyz") {
		require.True(t, letters[c])
	}
	var metadata struct {
		UnicodeVersion string `json:"unicode_version"`
		SourceURL      string `json:"source_url"`
		SourceSHA256   string `json:"source_sha256"`
		First          int    `json:"first_codepoint"`
		Last           int    `json:"last_codepoint"`
		Mapped         int    `json:"mapped_count"`
		Bytes          int    `json:"mapping_bytes"`
		SHA256         string `json:"mapping_sha256"`
	}
	data, err := os.ReadFile("compatibility_data/pinyin_metadata.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &metadata))
	require.Equal(t, "17.0.0", metadata.UnicodeVersion)
	require.Equal(t, "https://www.unicode.org/Public/17.0.0/ucd/Unihan.zip", metadata.SourceURL)
	require.Equal(t, "f7a48b2b545acfaa77b2d607ae28747404ce02baefee16396c5d2d7a8ef34b5e", metadata.SourceSHA256)
	require.Equal(t, 0x3400, metadata.First)
	require.Equal(t, 0x32389, metadata.Last)
	require.Equal(t, mapped, metadata.Mapped)
	require.Equal(t, len(builtinPinyinInitials), metadata.Bytes)
	require.Equal(t, expectedHash, metadata.SHA256)
}

func TestBuiltinPinyinAssetKnownReadings(t *testing.T) {
	// Hardcoded expectations come from the pinned kMandarin readings. Phrase
	// examples deliberately retain the first single-character reading.
	for _, tt := range []struct{ input, want string }{
		{"中国", "zg"},
		{"北京", "bj"},
		{"A张三1😀", "zs"},
		{"繁體測試", "ftcs"},
		{"重庆", "zq"},
		{"银行", "yx"},
		{"长安", "za"},
		{"龘𠀀", "dh"},
		{"㐀㐁\U00032389", "qtm"},
		{"", ""},
		{"ABC123😀", ""},
		{"𠮷〇㐂", ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			var got strings.Builder
			for _, c := range tt.input {
				if c < 0x3400 || c > 0x32389 {
					continue
				}
				if initial := builtinPinyinInitials[int(c)-0x3400]; initial != '?' {
					got.WriteByte(initial)
				}
			}
			require.Equal(t, tt.want, got.String())
		})
	}
}

func TestBuiltinFunctionDDLContracts(t *testing.T) {
	for _, name := range []string{"auto_increase_itemid", "count_rows", "fun_getpy"} {
		t.Run(name, func(t *testing.T) {
			ddl := builtinFunctionDDL("app", "pg_catalog", name)
			require.True(t, strings.HasPrefix(ddl, `CREATE FUNCTION "app"."`+name+`"(`))
			require.Contains(t, ddl, "LANGUAGE plpgsql")
			require.Contains(t, ddl, "SECURITY INVOKER")
			require.NotContains(t, ddl, "CREATE OR REPLACE")
			require.NotContains(t, ddl, "{{")
			require.NotContains(t, ddl, "CREATE TABLE")
			require.NotContains(t, ddl, "CREATE EXTENSION")
		})
	}
	require.Empty(t, builtinFunctionDDL("app", "pg_catalog", "unknown"))
	countRows := builtinFunctionDDL("app", "pg_catalog", "count_rows")
	require.Contains(t, countRows, "(table_name varchar, owner varchar DEFAULT NULL) RETURNS numeric")
	require.Contains(t, countRows, `"pg_catalog".quote_ident(table_name)`)
	require.Contains(t, countRows, `"pg_catalog".quote_ident(owner)`)
	require.NotContains(t, countRows, "SET search_path")
	auto := builtinFunctionDDL("app", "pg_catalog", "auto_increase_itemid")
	require.Contains(t, auto, `n.nspname=$dbgold$app$dbgold$`)
	require.Contains(t, auto, `"pg_catalog".lower(c.relname::text)='code_items'`)
	require.Contains(t, auto, `"pg_catalog".lower(a.attname::text)='itemid'`)
	require.Contains(t, auto, "num+($1-1)=itemid")
	require.Contains(t, auto, "INTO itemidfinal USING itemidtemp")
	require.Contains(t, auto, "RETURN CAST(itemidfinal AS varchar)")
	pinyin := builtinFunctionDDL("app", "pg_catalog", "fun_getpy")
	require.Contains(t, pinyin, "IMMUTABLE")
	require.Contains(t, pinyin, "UNICODE LICENSE V3")
	require.Contains(t, pinyin, builtinUnicodeLicense)
	require.Contains(t, pinyin, builtinPinyinInitials)
	require.Contains(t, pinyin, `"pg_catalog".convert_to("pg_catalog".substr(p_name,i,1),'UTF8')`)
	require.Contains(t, pinyin, `"pg_catalog".get_byte(word_bytes,3)`)
	require.Contains(t, pinyin, "RETURN nullif(result,'')")
	require.NotContains(t, pinyin, "ascii(")
	require.NotContains(t, pinyin, "nlssort")
}

func TestBuiltinFunctionDDLQuotesCapturedSchemaAndCatalog(t *testing.T) {
	const schema = `target'"\$$$dbgold$`
	const catalog = `native'"\$$$dbgold_1$`
	for _, name := range []string{"auto_increase_itemid", "count_rows", "fun_getpy"} {
		ddl := builtinFunctionDDL(schema, catalog, name)
		require.True(t, strings.HasPrefix(ddl, `CREATE FUNCTION "target'""\$$$dbgold$"."`+name+`"(`))
		require.Contains(t, ddl, `"native'""\$$$dbgold_1$".`)
		require.NotContains(t, ddl, "AS 'DECLARE")
	}
	auto := builtinFunctionDDL(schema, catalog, "auto_increase_itemid")
	require.Contains(t, auto, `n.nspname=$dbgold_1$target'"\$$$dbgold$$dbgold_1$`)
	// The outer quote must avoid both delimiters present in nested literals.
	require.Contains(t, auto, "SECURITY INVOKER AS $dbgold_2$DECLARE")
}
