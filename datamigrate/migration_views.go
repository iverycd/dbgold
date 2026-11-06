package datamigrate

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/source"
)

// Read source relation references without rewriting SQL. The shared view lexer
// handles vendor quoting and literals; query scopes keep CTE aliases local and
// keep expression FROM (EXTRACT/TRIM/...) separate from relation clauses.
func migrationViewDependencies(definition string, sourceTypes ...string) ([]string, error) {
	raw, err := lexOscarView(definition)
	if err != nil {
		return nil, err
	}
	var tokens []viewToken
	for _, token := range raw {
		if token.kind != 'w' {
			tokens = append(tokens, token)
		}
	}
	scanner := migrationViewScanner{tokens: tokens, close: map[int]int{}, seen: map[string]bool{}}
	if len(sourceTypes) > 0 {
		scanner.sourceType = sourceTypes[0]
	}
	var stack []int
	for i, token := range tokens {
		if token.text == "(" {
			stack = append(stack, i)
		}
		if token.text == ")" {
			if len(stack) == 0 {
				return nil, fmt.Errorf("视图定义括号不匹配")
			}
			scanner.close[stack[len(stack)-1]] = i
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("视图定义括号不匹配")
	}
	if err := scanner.scan(0, len(tokens), map[string]bool{}, false); err != nil {
		return nil, err
	}
	return scanner.names, nil
}

type migrationViewScanner struct {
	sourceType string
	tokens     []viewToken
	close      map[int]int
	seen       map[string]bool
	names      []string
}

func (s *migrationViewScanner) word(i int) string {
	if i < 0 || i >= len(s.tokens) || s.tokens[i].kind != 'i' {
		return ""
	}
	return strings.ToUpper(s.tokens[i].name)
}
func (s *migrationViewScanner) ident(i int) bool {
	return i >= 0 && i < len(s.tokens) && (s.tokens[i].kind == 'i' || s.tokens[i].kind == 'q')
}
func (s *migrationViewScanner) cteKey(token viewToken) string {
	if s.sourceType == "sqlserver" {
		return strings.ToLower(token.name)
	}
	if token.kind == 'q' {
		return token.name
	}
	if s.sourceType == "oracle" || s.sourceType == "dameng" || s.sourceType == "oscar" {
		return strings.ToUpper(token.name)
	}
	return strings.ToLower(token.name)
}
func cloneMigrationCTEs(original map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(original))
	for key, value := range original {
		copy[key] = value
	}
	return copy
}

// Parse a WITH header before visiting its bodies. Non-recursive definitions see
// preceding CTEs and outer scopes; qualified relations always remain real names.
func (s *migrationViewScanner) ctes(start, end int, inherited map[string]bool) (int, map[string]bool, error) {
	type cte struct {
		name        string
		first, last int
	}
	i := start + 1
	explicitRecursive := s.word(i) == "RECURSIVE"
	recursive := explicitRecursive || s.sourceType == "sqlserver" || s.sourceType == "oracle"
	if explicitRecursive {
		i++
	}
	var definitions []cte
	for i < end && s.ident(i) {
		name := s.cteKey(s.tokens[i])
		i++
		if i < end && s.tokens[i].text == "(" {
			i = s.close[i] + 1
		}
		if s.word(i) != "AS" {
			return start, inherited, nil
		}
		i++
		if s.word(i) == "NOT" {
			i++
		}
		if s.word(i) == "MATERIALIZED" {
			i++
		}
		if i >= end || s.tokens[i].text != "(" {
			return start, inherited, nil
		}
		close := s.close[i]
		definitions = append(definitions, cte{name: name, first: i + 1, last: close})
		i = close + 1
		if i >= end || s.tokens[i].text != "," {
			break
		}
		i++
	}
	if len(definitions) == 0 {
		return start, inherited, nil
	}
	scope := cloneMigrationCTEs(inherited)
	if recursive {
		for _, definition := range definitions {
			scope[definition.name] = true
		}
	}
	for _, definition := range definitions {
		if err := s.scan(definition.first, definition.last, scope, false); err != nil {
			return i, scope, err
		}
		scope[definition.name] = true
	}
	return i, scope, nil
}

func (s *migrationViewScanner) scan(start, end int, inherited map[string]bool, relationGroup bool) error {
	scope := inherited
	query, from, expect := relationGroup, relationGroup, relationGroup
	for i := start; i < end; i++ {
		word := s.word(i)
		if word == "WITH" {
			next, inner, err := s.ctes(i, end, scope)
			if err != nil {
				return err
			}
			if next != i {
				scope = inner
				i = next - 1
				query = true
				from = false
				expect = false
				continue
			}
		}
		if s.tokens[i].text == "(" {
			close := s.close[i]
			// A relation grouping inherits FROM; ordinary expression/function
			// parentheses only acquire it through a nested SELECT of their own.
			group := expect
			expect = false
			if err := s.scan(i+1, close, scope, group); err != nil {
				return err
			}
			i = close
			continue
		}
		if word == "SELECT" {
			query = true
			from = false
			expect = false
			continue
		}
		if query && (word == "FROM" || word == "JOIN" || word == "APPLY" || word == "STRAIGHT_JOIN") {
			from = true
			expect = true
			continue
		}
		switch word {
		case "WHERE", "GROUP", "HAVING", "ORDER", "UNION", "EXCEPT", "INTERSECT", "LIMIT", "FETCH", "OFFSET", "QUALIFY", "WINDOW", "CONNECT", "START", "RETURNING":
			from = false
			expect = false
			continue
		}
		if s.tokens[i].text == "," && from {
			expect = true
			continue
		}
		if !expect {
			continue
		}
		if word == "ONLY" || word == "LATERAL" {
			continue
		}
		expect = false
		if !s.ident(i) {
			continue
		}
		first, last := i, i
		for last+2 < end && s.tokens[last+1].text == "." && s.ident(last+2) {
			last += 2
		}
		// A table-valued function is not a table name. Its argument expressions
		// may still contain SELECT subqueries, which are visited independently.
		if last+1 < end && s.tokens[last+1].text == "(" {
			close := s.close[last+1]
			if err := s.scan(last+2, close, scope, false); err != nil {
				return err
			}
			i = close
			continue
		}
		i = last
		if first == last && scope[s.cteKey(s.tokens[last])] {
			continue
		}
		name := s.tokens[last].name
		if !s.seen[name] {
			s.names = append(s.names, name)
			s.seen[name] = true
		}
	}
	return nil
}

func (m *Migrator) migrateIndependentViews(ctx context.Context, views []source.ViewInfo, category *CategoryReport) {
	byTarget := map[string]string{}
	for _, view := range views {
		target := m.objName(view.ViewName)
		if previous, exists := byTarget[target]; exists {
			err := fmt.Errorf("目标视图名冲突: %q 与 %q 均映射为 %q", previous, view.ViewName, target)
			for _, name := range []string{previous, view.ViewName} {
				ref := ObjectRef{Kind: "view", Name: name}
				if m.migrationObjectError(ref) == nil {
					m.blockMigrationObject(ref, err)
				}
			}
		}
		byTarget[target] = view.ViewName
	}
	dependencies := map[string][]string{}
	pending := map[string]source.ViewInfo{}
	for _, view := range views {
		if err := m.migrationObjectError(ObjectRef{Kind: "view", Name: view.ViewName}); err != nil {
			category.Failed++
			category.Items = append(category.Items, ObjectResult{Name: m.objName(view.ViewName), SourceName: view.ViewName, TargetName: m.objName(view.ViewName), Stage: "preflight", Error: err.Error()})
			continue
		}
		deps, err := migrationViewDependencies(view.Definition, m.reader.DBType())
		if err != nil {
			m.blockMigrationObject(ObjectRef{Kind: "view", Name: view.ViewName}, err)
			category.Failed++
			category.Items = append(category.Items, ObjectResult{Name: m.objName(view.ViewName), SourceName: view.ViewName, TargetName: m.objName(view.ViewName), Stage: "preflight", Error: err.Error()})
			continue
		}
		dependencies[view.ViewName], pending[view.ViewName] = deps, view
	}
	for len(pending) > 0 {
		progress := false
		for _, original := range views {
			view, exists := pending[original.ViewName]
			if !exists {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			waiting := false
			var blocked error
			for _, dep := range dependencies[view.ViewName] {
				if err := m.migrationTargetTableError(m.objName(dep)); err != nil {
					blocked = fmt.Errorf("依赖表 %s 迁移失败: %w", dep, err)
					break
				}
				if name, found := byTarget[m.objName(dep)]; found {
					if err := m.migrationObjectError(ObjectRef{Kind: "view", Name: name}); err != nil {
						blocked = fmt.Errorf("依赖视图 %s 迁移失败: %w", dep, err)
						break
					}
					if _, queued := pending[name]; queued {
						waiting = true
					}
				}
			}
			if blocked != nil {
				m.blockMigrationObject(ObjectRef{Kind: "view", Name: view.ViewName}, blocked)
				m.recordBlockedObject(category, m.objName(view.ViewName), "", blocked.Error())
			} else if waiting {
				continue
			} else {
				ddl, err := m.createOneView(ctx, view)
				if err != nil {
					m.blockMigrationObject(ObjectRef{Kind: "view", Name: view.ViewName}, err)
					category.Failed++
					result := ObjectResult{Name: m.objName(view.ViewName), SourceName: view.ViewName, TargetName: m.objName(view.ViewName), Stage: "execute", DDL: ddl, Error: err.Error()}
					m.attachSQLServerViewIssue(&result, view, err)
					category.Items = append(category.Items, result)
					if m.stopMigrationTask(ctx, err) {
						return
					}
				} else {
					category.Success++
				}
			}
			delete(pending, view.ViewName)
			progress = true
		}
		if !progress {
			for _, view := range views {
				if _, exists := pending[view.ViewName]; !exists {
					continue
				}
				err := fmt.Errorf("视图依赖循环或依赖未完成")
				m.blockMigrationObject(ObjectRef{Kind: "view", Name: view.ViewName}, err)
				m.recordBlockedObject(category, m.objName(view.ViewName), "", err.Error())
			}
			return
		}
	}
}

func (m *Migrator) preflightMigrationNames(tables []string) {
	names := map[string]string{}
	for _, table := range tables {
		name := m.objName(table)
		if previous, exists := names[name]; exists && previous != table {
			err := fmt.Errorf("目标表名冲突: %q 与 %q 均映射为 %q", previous, table, name)
			m.blockMigrationTable(previous, "preflight", err)
			m.blockMigrationTable(table, "preflight", err)
		}
		names[name] = table
	}
}
