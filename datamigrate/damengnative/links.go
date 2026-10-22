package damengnative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	linkPasswordMarker = `'__DBGOLD_DBLINK_CREDENTIAL__'`
	linkConnectMarker  = `'__DBGOLD_DBLINK_CONNECTION__'`
	linkMarkerPrefix   = "__DBGOLD_DBLINK_"
)

type linkSecret struct{ marker, sql string }

// LinkDDLReader is optional: other database readers do not acquire any new
// methods. It is also used after restart to rehydrate a credential-free ledger.
type LinkDDLReader interface {
	ReadDamengNativeLinkDDL(context.Context, string) (string, error)
}

func linkHash(sql string) string {
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}

// linkParts accepts the documented DM CREATE LINK grammar. Comments and opaque
// literal values are omitted from its public form. In particular, USING may be
// an ODBC connection string or URI containing a second password.
func linkParts(ddl string) (public string, secrets []linkSecret, err error) {
	ddl = StripClientDelimiter(ddl)
	tokens, err := nativeTokens(ddl)
	if err != nil {
		return "", nil, fmt.Errorf("DBLink 原生定义不能解析")
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].kind == 'p' && tokens[len(tokens)-1].value == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) < 3 || !strings.EqualFold(tokens[0].value, "CREATE") || !LooksLikeLinkDDL(ddl) {
		return "", nil, fmt.Errorf("DBLink 缺少 CREATE 定义")
	}
	by, using, identified, link := -1, -1, -1, false
	for i, t := range tokens {
		if t.kind == 'p' && t.value == ";" {
			return "", nil, fmt.Errorf("DBLink 定义包含额外执行单元")
		}
		if t.kind != 'i' {
			continue
		}
		switch strings.ToUpper(t.value) {
		case "PUBLIC":
			if !link {
				return "", nil, fmt.Errorf("公有 DBLink 不属于所选 Schema")
			}
		case "LINK":
			link = true
		case "IDENTIFIED":
			if identified >= 0 {
				return "", nil, fmt.Errorf("DBLink 包含重复凭据子句")
			}
			identified = i
		case "BY":
			if i == identified+1 && identified >= 0 {
				by = i
			}
		case "USING":
			if by >= 0 && using < 0 {
				using = i
			}
		}
	}
	if !link || by < 0 || using <= by+1 || using+1 >= len(tokens) || tokens[using+1].kind != 's' {
		return "", nil, fmt.Errorf("DBLink 缺少可完整读取的登录凭据或连接串")
	}
	// DM does not document Oracle's IDENTIFIED BY VALUES form. Masked/bind
	// placeholders cannot establish a working target link and must fail early.
	if strings.EqualFold(tokens[by+1].value, "VALUES") {
		return "", nil, fmt.Errorf("DBLink 凭据为不可移植的口令占位或密文格式")
	}
	password := strings.TrimSpace(ddl[tokens[by+1].start:tokens[using-1].end])
	value := tokens[by+1].value
	if password == "" || strings.Trim(value, "*? ") == "" || tokens[by+1].kind == 'p' && (value == ":" || value == "?") || strings.HasPrefix(value, linkMarkerPrefix) {
		return "", nil, fmt.Errorf("DBLink 原生元数据未返回可回放的完整凭据")
	}
	var parts []string
	for i := 0; i < len(tokens); i++ {
		if i == by+1 {
			parts = append(parts, linkPasswordMarker)
			secrets = append(secrets, linkSecret{linkPasswordMarker, password})
			i = using - 1
			continue
		}
		if tokens[i].kind == 's' {
			if strings.HasPrefix(tokens[i].value, linkMarkerPrefix) {
				return "", nil, fmt.Errorf("DBLink 原生元数据包含不可执行的脱敏标记")
			}
			marker := fmt.Sprintf("'__DBGOLD_DBLINK_LITERAL_%d__'", i)
			if i == using+1 {
				marker = linkConnectMarker
			}
			parts = append(parts, marker)
			secrets = append(secrets, linkSecret{marker, ddl[tokens[i].start:tokens[i].end]})
			continue
		}
		parts = append(parts, ddl[tokens[i].start:tokens[i].end])
	}
	return strings.Join(parts, " "), secrets, nil
}

func FreezeLinkUnit(ddl, phase string) (Unit, error) {
	ddl = StripClientDelimiter(ddl)
	public, secrets, err := linkParts(ddl)
	if err != nil {
		return Unit{}, err
	}
	actual, err := injectLinkSecrets(public, secrets)
	if err != nil {
		return Unit{}, err
	}
	return Unit{SQL: public, Phase: phase, SourceSQLHash: linkHash(ddl), executableSQL: actual}, nil
}

func injectLinkSecrets(public string, secrets []linkSecret) (string, error) {
	tokens, err := nativeTokens(public)
	if err != nil {
		return "", fmt.Errorf("DBLink 公开定义不能解析")
	}
	replacements := map[string]string{}
	for _, secret := range secrets {
		replacements[secret.marker] = secret.sql
	}
	var b strings.Builder
	start, replaced := 0, 0
	for _, token := range tokens {
		if token.kind != 's' || !strings.HasPrefix(token.value, linkMarkerPrefix) {
			continue
		}
		actual, exists := replacements[public[token.start:token.end]]
		if !exists {
			return "", fmt.Errorf("DBLink 公开定义包含未知凭据标记")
		}
		delete(replacements, public[token.start:token.end])
		b.WriteString(public[start:token.start])
		b.WriteString(actual)
		start, replaced = token.end, replaced+1
	}
	if len(replacements) != 0 || replaced == 0 {
		return "", fmt.Errorf("DBLink 公开定义缺少凭据标记")
	}
	b.WriteString(public[start:])
	return b.String(), nil
}

// RewriteUnit maps the redacted SQL, then reinstates the password unchanged.
// Remote credentials and connection strings never participate in Schema mapping.
func RewriteUnit(u Unit, source, target string) (Unit, []string, error) {
	mapped, warnings, err := RewriteSchema(u.SQL, source, target)
	if err != nil {
		return Unit{}, nil, err
	}
	if u.SourceSQLHash != "" && u.executableSQL != "" {
		_, secrets, e := linkParts(u.executableSQL)
		if e != nil {
			return Unit{}, nil, e
		}
		u.executableSQL, err = injectLinkSecrets(mapped, secrets)
		if err != nil {
			return Unit{}, nil, err
		}
	}
	u.SQL = mapped
	return u, warnings, nil
}

func HydrateLinkUnit(u Unit, ddl, source, target string) (Unit, error) {
	fresh, err := FreezeLinkUnit(ddl, u.Phase)
	if err != nil {
		return Unit{}, err
	}
	if fresh.SourceSQLHash != u.SourceSQLHash || u.SourceSQLHash == "" {
		return Unit{}, fmt.Errorf("源 DBLink 定义或凭据已变化，禁止重放旧计划")
	}
	fresh, _, err = RewriteUnit(fresh, source, target)
	if err != nil || fresh.SQL != u.SQL {
		return Unit{}, fmt.Errorf("DBLink 定义与冻结的目标执行单元不符")
	}
	return fresh, nil
}

type linkResolverKey struct{}
type LinkResolver func(context.Context, Object, Unit) (Unit, error)

func WithLinkResolver(ctx context.Context, resolver LinkResolver) context.Context {
	return context.WithValue(ctx, linkResolverKey{}, resolver)
}

func WithSourceLinkResolver(ctx context.Context, reader LinkDDLReader, sourceSchema string) context.Context {
	return WithLinkResolver(ctx, func(ctx context.Context, object Object, unit Unit) (Unit, error) {
		ddl, err := reader.ReadDamengNativeLinkDDL(ctx, object.Name)
		if err != nil {
			return Unit{}, fmt.Errorf("重新读取源 DBLink 冻结定义失败")
		}
		return HydrateLinkUnit(unit, ddl, sourceSchema, object.Schema)
	})
}

// ExecutionSQL cannot accidentally execute the redaction marker after a
// process restart. Hydration is mandatory, with the original source hash.
func ExecutionSQL(ctx context.Context, object Object, unit Unit) (string, error) {
	if unit.SourceSQLHash == "" {
		return unit.SQL, nil
	}
	if object.Kind != "db_link" {
		return "", fmt.Errorf("凭据执行单元不属于 DBLink")
	}
	if unit.executableSQL == "" {
		resolver, ok := ctx.Value(linkResolverKey{}).(LinkResolver)
		if !ok {
			return "", fmt.Errorf("DBLink 重试需要重新读取并核对源库冻结凭据")
		}
		frozen := unit
		var err error
		unit, err = resolver(ctx, object, unit)
		if err != nil {
			return "", fmt.Errorf("重新核对 DBLink 冻结定义或凭据失败")
		}
		if unit.SQL != frozen.SQL || unit.SourceSQLHash != frozen.SourceSQLHash || unit.Phase != frozen.Phase {
			return "", fmt.Errorf("DBLink 重新读取结果与冻结执行单元不符")
		}
	}
	if unit.executableSQL == "" {
		return "", fmt.Errorf("DBLink 凭据不可用")
	}
	public, _, err := linkParts(unit.executableSQL)
	if err != nil || public != unit.SQL {
		return "", fmt.Errorf("DBLink 执行凭据与冻结公开定义不符")
	}
	return unit.executableSQL, nil
}

// LooksLikeLinkDDL fails closed before a raw CREATE/ALTER LINK can fall through
// to an ordinary observer. It also recognizes a malformed quoted tail, since
// that tail itself can contain the password which caused parsing to fail.
func LooksLikeLinkDDL(sql string) bool {
	first := true
	for i := 0; i < len(sql); {
		r, n := utf8.DecodeRuneInString(sql[i:])
		if unicode.IsSpace(r) {
			i += n
			continue
		}
		if strings.HasPrefix(sql[i:], "--") {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(sql[i:], "/*") {
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) {
			return false
		}
		start := i
		for i < len(sql) {
			r, n = utf8.DecodeRuneInString(sql[i:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' && r != '#' {
				break
			}
			i += n
		}
		word := strings.ToUpper(sql[start:i])
		if first {
			if word != "CREATE" && word != "ALTER" {
				return false
			}
			first = false
			continue
		}
		switch word {
		case "OR", "REPLACE", "PUBLIC", "PRIVATE", "DATABASE", "FORCE", "UNIQUE", "BITMAP", "CLUSTERED", "NONCLUSTERED", "GLOBAL", "LOCAL", "SPATIAL":
			continue
		case "LINK", "DBLINK":
			return true
		default:
			return false
		}
	}
	return false
}

// LinkUnitsEquivalent compares ephemeral credentials only in memory. Public
// SQL alone cannot prove that a pre-existing link has the correct password or
// opaque connection options. Restarted units must be hydrated before this proof.
func LinkUnitsEquivalent(a, b Unit) bool {
	if a.SourceSQLHash == "" || b.SourceSQLHash == "" || a.executableSQL == "" || b.executableSQL == "" {
		return false
	}
	at, ae := nativeTokens(a.SQL)
	bt, be := nativeTokens(b.SQL)
	if ae != nil || be != nil || len(at) != len(bt) {
		return false
	}
	for i := range at {
		if at[i].kind != bt[i].kind || at[i].value != bt[i].value && !(at[i].kind == 'i' && strings.EqualFold(at[i].value, bt[i].value)) {
			return false
		}
	}
	_, x, err := linkParts(a.executableSQL)
	if err != nil {
		return false
	}
	_, y, err := linkParts(b.executableSQL)
	if err != nil || len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// PublicLinkSQL is also used at the execution boundary to sanitize observers
// and driver errors before they can enter durable state.
func PublicLinkSQL(sql string) (string, error) {
	public, _, err := linkParts(sql)
	return public, err
}

// ExecuteUnit keeps sensitive SQL out of generic writer/observer entry points.
func ExecuteUnit(ctx context.Context, writer Writer, object Object, unit Unit) error {
	switch unit.Phase {
	case "mv_rebase":
		writer, ok := writer.(interface {
			RebaseDamengNativeMaterialized(context.Context, Object) error
		})
		if !ok {
			return fmt.Errorf("写入器缺少物化视图基线核验能力")
		}
		return writer.RebaseDamengNativeMaterialized(ctx, object)
	case "event_create":
		writer, ok := writer.(interface {
			InstallDamengNativeEvent(context.Context, Object, Unit) error
		})
		if !ok {
			return fmt.Errorf("写入器缺少事件触发器受控安装能力")
		}
		return writer.InstallDamengNativeEvent(ctx, object, unit)
	case "event_activate":
		writer, ok := writer.(interface {
			SetDamengNativeEventState(context.Context, Object, Unit) error
		})
		if !ok {
			return fmt.Errorf("写入器缺少事件触发器最终状态恢复能力")
		}
		return writer.SetDamengNativeEventState(ctx, object, unit)
	}
	actual, err := ExecutionSQL(ctx, object, unit)
	if err != nil {
		return err
	}
	if unit.SourceSQLHash == "" {
		return writer.ExecuteDamengNative(ctx, actual)
	}
	protected, ok := writer.(interface {
		ExecuteDamengNativeSensitive(context.Context, string, string) error
	})
	if !ok {
		return fmt.Errorf("DBLink 写入器缺少凭据脱敏执行能力")
	}
	return protected.ExecuteDamengNativeSensitive(ctx, unit.SQL, actual)
}
