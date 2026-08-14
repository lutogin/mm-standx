package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Правила, которые нельзя доверить внимательности.
//
// Оба закрывают отказы, уже случившиеся на живом счёте, и оба ломаются правкой,
// выглядящей безобидно: «подписать стакан на общую очередь, как всё остальное»
// и «взять время напрямую, тут же одна строчка». Ревью такое пропускает —
// правка выглядит как приведение к единообразию, а не как регресс.

// pkgFiles разбирает несобственно-тестовые файлы пакета.
func pkgFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)

	pkg, ok := pkgs["engine"]
	require.True(t, ok, "package engine must parse")
	return fset, pkg.Files
}

// Стакан и внешний ориентир обязаны идти мимо очереди актора.
//
// Пока они делили очередь с исполнениями и обновлениями ордеров, поток в 3.5
// сообщения в секунду занимал те же 1024 слота, что и события о собственных
// ордерах. 13 августа он их занял, событие счёта не поместилось, актор
// остановился до конца прогона.
//
// Устаревший снимок стакана не нужен никому: следующее сообщение приносит ту же
// величину заново. Исполнение — нужно, и восстанавливается только сверкой.
// Класть их в одну очередь означает поменять первое на второе под нагрузкой.
func TestFastStreamsDoNotEnterTheActorQueue(t *testing.T) {
	fset, files := pkgFiles(t)

	const fast = "BookUpdated', 'ReferencePriceUpdated"
	seen := map[string]bool{}

	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || lit.Type.Params == nil {
				return true
			}
			// Обработчик шины: func(context.Context, events.X).
			event := subscriberEventName(lit)
			if event != "BookUpdated" && event != "ReferencePriceUpdated" {
				return true
			}
			seen[event] = true

			require.False(t, callsMethod(lit.Body, "enqueue"),
				"%s: events.%s must not be enqueued — it belongs in the latest-value slot, "+
					"otherwise the market firehose can starve own-account events again (%s)",
				fset.Position(lit.Pos()), event, fast)
			// Swap или Store: первый вдобавок считает затёртое непрочитанное,
			// и именно он стоит в подписчиках. Второй остаётся допустимым —
			// правило про то, куда кладут, а не про учёт потерь.
			intoSlot := callsMethod(lit.Body, "Swap") || callsMethod(lit.Body, "Store")
			require.True(t, intoSlot && callsMethod(lit.Body, "notify"),
				"%s: events.%s must go into the latest-value slot and be announced with notify()",
				fset.Position(lit.Pos()), event)
			return true
		})
	}

	require.True(t, seen["BookUpdated"], "the BookUpdated subscriber must exist to be checked")
	require.True(t, seen["ReferencePriceUpdated"], "the ReferencePriceUpdated subscriber must exist")
}

// Ячейки быстрых потоков разбираются до выбора, а не наравне с ним.
//
// Это ограничение порядка, и проверяется оно структурно, потому что нарушение
// проявляется не отказом, а смещением: Go выбирает среди готовых случаев
// равновероятно, поэтому `case <-e.wake` наравне с `case <-e.inbox` означает,
// что такт mark может обработаться, пока свежий стакан лежит непрочитанным.
// onMarkPrice ставит из полей актора обе опорные точки сноса, и якорь
// от устаревшего стакана даёт снос, которого не было, — то есть аварийный съём
// на ровном месте. Каналы price и depth_book идут по одному соединению, и
// до разделения входа очередь давала этот порядок даром.
//
// Тайминговый тест такое не ловит: он зелёный на любом конкретном прогоне
// и краснеет раз в сто запусков.
func TestMarketCellsAreDrainedBeforeTheSelect(t *testing.T) {
	fset, files := pkgFiles(t)

	var loop *ast.FuncDecl
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == "loop" && fn.Recv != nil {
				loop = fn
			}
		}
	}
	require.NotNil(t, loop, "Engine.loop must exist to be checked")

	var forBody *ast.BlockStmt
	ast.Inspect(loop.Body, func(n ast.Node) bool {
		if f, ok := n.(*ast.ForStmt); ok && forBody == nil {
			forBody = f.Body
		}
		return forBody == nil
	})
	require.NotNil(t, forBody, "the actor loop must be a for loop")

	drainAt, selectAt := -1, -1
	for i, stmt := range forBody.List {
		if _, ok := stmt.(*ast.SelectStmt); ok && selectAt < 0 {
			selectAt = i
		}
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "onMarket" && drainAt < 0 {
				drainAt = i
			}
			return true
		})
	}

	require.GreaterOrEqual(t, drainAt, 0,
		"%s: the loop must drain the market cells every iteration", fset.Position(loop.Pos()))
	require.GreaterOrEqual(t, selectAt, 0, "the loop must select")
	require.Less(t, drainAt, selectAt,
		"%s: onMarket must run BEFORE the select, not as a peer case — "+
			"a peer case lets a mark be handled while a newer book sits unread, "+
			"and onMarkPrice anchors the drift estimate on e.book",
		fset.Position(loop.Pos()))
}

// Время берётся только через порт ports.Clock.
//
// Прямой time.Now делает тесты недетерминированными, а time.Sleep — ещё и
// блокирует горутину актора, то есть перестаёт разбирать очередь. Ровно эта
// пара свойств превратила 13 августа замедление в остановку.
func TestActorTakesTimeOnlyFromTheClockPort(t *testing.T) {
	fset, files := pkgFiles(t)

	banned := map[string]string{
		"Now":       "use deps.Clock.Now()",
		"Sleep":     "blocking the actor goroutine stops it draining its queue",
		"After":     "use deps.Clock.After()",
		"Tick":      "use deps.Clock.NewTicker()",
		"NewTicker": "use deps.Clock.NewTicker()",
		"NewTimer":  "use deps.Clock.After()",
		"Since":     "use deps.Clock.Now().Sub()",
	}

	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "time" {
				return true
			}
			why, forbidden := banned[sel.Sel.Name]
			require.Falsef(t, forbidden,
				"%s: time.%s is forbidden in the actor package — %s",
				fset.Position(sel.Pos()), sel.Sel.Name, why)
			return true
		})
	}
}

// subscriberEventName возвращает имя типа события обработчика шины,
// то есть X из func(context.Context, events.X). Пусто для прочих литералов.
func subscriberEventName(lit *ast.FuncLit) string {
	params := lit.Type.Params.List
	if len(params) != 2 {
		return ""
	}
	sel, ok := params[1].Type.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "events" {
		return ""
	}
	return sel.Sel.Name
}

// callsMethod сообщает, вызывается ли где-либо в теле метод с таким именем.
func callsMethod(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				found = true
			}
		case *ast.Ident:
			if fn.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// Блокирующие вызовы на пути актора заморожены поимённо.
//
// Актор ходит в сеть своей же горутиной и на время похода не разбирает очередь
// (PLAN.md §0, дефект 5). Вынести I/O наружу — отдельная работа; до тех пор
// единственное, что можно гарантировать, — что новых таких мест не появится.
//
// Правило укореняется положительно, от четырёх точек входа горутины актора.
// Соблазнительная формулировка «всё, до чего дотягивается Stop, не считается»
// неверна ровно наоборот: Stop дотягивается до withdraw и flatten, то есть
// как раз до тех функций, где горячие вызовы и живут.
func TestBlockingCallsOnTheActorPathAreFrozen(t *testing.T) {
	fset, files := pkgFiles(t)

	// Разрешено сегодня. Ключ — «функция -> вызов», не файл со строкой:
	// номера строк ползут от любой правки и превратили бы список в шум.
	allowed := map[string]bool{
		"execute -> Cancel":             true,
		"place -> Place":                true,
		"sendWithdrawal -> Cancel":      true,
		"submitFlatten -> Place":        true,
		"reconcile -> CancelBatch":      true,
		"clearRemaining -> Sleep":       true,
		"sendWithdrawal -> CancelBatch": true,
	}
	// Что считается блокирующим: поход наружу или сон.
	sinks := map[string]bool{
		"Place": true, "Cancel": true, "CancelBatch": true, "Sleep": true,
	}

	methods := map[string]*ast.FuncDecl{}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Body != nil {
				methods[fn.Name.Name] = fn
			}
		}
	}

	// Обход от точек входа горутины актора.
	seen := map[string]bool{}
	queue := []string{"loop", "handle", "onTick", "onMarket"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true

		fn, ok := methods[name]
		if !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			callee := sel.Sel.Name

			if sinks[callee] {
				key := name + " -> " + callee
				require.Truef(t, allowed[key],
					"%s: %s reaches a blocking %s from the actor goroutine. "+
						"While the actor blocks it stops draining its queue — that is how "+
						"13 August turned a slowdown into an outage. If this call is "+
						"deliberate, add %q to the allowlist and say why.",
					fset.Position(call.Pos()), name, callee, key)
			}
			// Свои же методы: продолжаем обход.
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "e" {
				queue = append(queue, callee)
			}
			return true
		})
	}

	require.True(t, seen["evaluate"], "the walk must reach evaluate, or it proves nothing")
}
