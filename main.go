package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antlr/antlr4/runtime/Go/antlr"
	parser "github.com/nycmonkey/sprocs/tsql"
	pb "gopkg.in/cheggaaa/pb.v1"

	_ "github.com/denisenkom/go-mssqldb"
)

var (
	dbHost       string
	bar          *pb.ProgressBar
	faster       bool
	activeSprocQ = `
SELECT ResultsStoredProc
FROM [BRS].[dbo].[BRS_ConfigDrivenETLs]
ORDER BY ResultsStoredProc ASC
`
	sprocQ = `
SELECT OBJECT_DEFINITION (OBJECT_ID(?))
`
	tableQ = `
SELECT TABLE_NAME FROM BRS.INFORMATION_SCHEMA.Tables WHERE TABLE_SCHEMA = 'dbo'
`
	portfolioQ = `
SELECT [PortfolioShortName]
       ,[GuggenheimUnitShortName]
       ,[RelationshipShortName]
       ,[ClientShortName]
       ,[AccountShortName]
       ,[PortfolioCode]
    
  FROM [BRS].[dbo].[vw_AMPortfolioMaster]
`
	outDir        string
	whitelist     map[string]bool
	portfolioKeys map[string]bool
)

// TreeShapeListener handles events from a TSQL parser generated by Antlr
type TreeShapeListener struct {
	*parser.BasetsqlListener
	inProcDef        bool
	info             *SprocInfo
	tablesUsedCh     chan<- string
	portfolioCodesCh chan<- string
}

// SprocInfo is a structure to record stored procedure metadata
type SprocInfo struct {
	Name       string
	Tables     map[string]bool
	Aliases    map[string]bool
	Portfolios map[string]bool
}

// emailAccount captures sproc report recipient details looked up by email address in CORP DB
type emailAccount struct {
	FirstName       string
	LastName        string
	LegalEntityName string
	BusinessUnit    int
	BUText          string
	Position        string
	Location        string
	DirectManager   string
}

type keyValue struct {
	key, value string
}

// errorListener extends the default error listener generated by antlr to send TSQL syntax errors down a channel
type errorListener struct {
	*antlr.DefaultErrorListener
	errCh     chan<- keyValue
	sprocName string
}

func init() {
	flag.StringVar(&dbHost, "host", "IL1TSTSQL10", "sproc database host server name")
	flag.BoolVar(&faster, "fast", false, "use a faster but less error-tolerant parsing strategy")
	whitelist = make(map[string]bool)
	portfolioKeys = make(map[string]bool)
}

func main() {
	flag.Parse()
	outDir = outDirPath()
	defDir := filepath.Join(outDir, `sproc_definitions`)
	err := os.MkdirAll(defDir, os.ModeDir)
	if err != nil {
		log.Fatalln("Couldn't create output directory:", err)
	}
	log.Println("Writing output to", outDir)
	sprocCh := make(chan keyValue)
	tablesCh := make(chan []string, 1)
	portfoliosCh := make(chan []string, 1)
	tablesHandled := make(chan bool)
	portfoliosHandled := make(chan bool)
	errorsHandled := make(chan bool)
	errCh := make(chan []string, 1)
	go handleTables(tablesCh, tablesHandled)
	go handlePortfolios(portfoliosCh, portfoliosHandled)
	go handleErrors(errCh, errorsHandled)
	wg := new(sync.WaitGroup)
	for i := 0; i < 6; i++ {
		// spin up a bunch of concurrent sproc parsing routines, and watch the CPU burn
		wg.Add(1)
		go handleSprocDetails(defDir, sprocCh, tablesCh, portfoliosCh, errCh, wg)
	}
	err = getSprocs(defDir, sprocCh)
	if err != nil {
		log.Fatalln("error querying", dbHost+":", err)
	}
	wg.Wait() // this can take a while
	close(tablesCh)
	close(errCh)
	close(portfoliosCh)
	<-tablesHandled
	<-errorsHandled
	<-portfoliosHandled
	bar.FinishPrint("All sprocs parsed")
}

func outDirPath() string {
	return fmt.Sprintf("%s_%s", time.Now().Format(`2006-01-02`), dbHost)
}

func getSprocs(defDir string, outCh chan<- keyValue) error {
	log.Println("Querying", dbHost)
	defer close(outCh)
	db, err := sql.Open("mssql", "server="+dbHost+";database=BRS;ApplicationIntent=ReadOnly")
	if err != nil {
		log.Fatalln(err)
	}
	defer db.Close()
	log.Println("Fetching list of known tables")
	fmt.Println(tableQ)
	rows, err := db.Query(tableQ)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tableName string
		if err = rows.Scan(&tableName); err != nil {
			rows.Close()
			return err
		}
		whitelist[strings.ToUpper(strings.TrimSpace(tableName))] = true
	}
	rows.Close()
	log.Println("Loaded table whitelist with", len(whitelist), "values")

	log.Println("Fetching account / portfolio identifiers")
	{
		rows, err := db.Query(portfolioQ)
		if err != nil {
			return err
		}
		var psn, gusn, rsn, csn, asn sql.NullString
		var pc sql.NullInt64
		for rows.Next() {
			if err = rows.Scan(&psn, &gusn, &rsn, &csn, &asn, &pc); err != nil {
				rows.Close()
				return err
			}
			for _, code := range []sql.NullString{psn, gusn, rsn, csn, asn} {
				if code.Valid {
					portfolioKeys[code.String] = true
				}
			}
			if pc.Valid {
				portfolioKeys[fmt.Sprintf("%d", pc.Int64)] = true
			}
		}
		rows.Close()
	}
	log.Println("Loaded", len(portfolioKeys), "unique portfolio codes")

	log.Println("Looking up active stored procedures")
	fmt.Println(activeSprocQ)
	rows, err = db.Query(activeSprocQ)
	if err != nil {
		return err
	}
	defer rows.Close()
	var sprocNames []string
	for rows.Next() {
		var sprocName sql.NullString
		if err = rows.Scan(&sprocName); err != nil {
			rows.Close()
			return err
		}
		if sprocName.Valid {
			sprocNames = append(sprocNames, sprocName.String)
		}
	}
	rows.Close()
	log.Println("Found", len(sprocNames), "active stored procedures")
	var def sql.NullString

	// fetch sproc definitions
	log.Println("Fetching stored procedure definitions")
	fmt.Println(sprocQ)
	validIndices := make([]int, 0, len(sprocNames))
	for i, sn := range sprocNames {
		err := db.QueryRow(sprocQ, `BRS.dbo.`+sn).Scan(&def)
		if err != nil {
			return errors.New("error while querying definition of " + sn + ": " + err.Error())
		}
		if !def.Valid {
			log.Println("No definition found for", sn)
			continue
		}
		validIndices = append(validIndices, i)
		var f *os.File
		f, err = os.Create(filepath.Join(defDir, sn+".sql"))
		if err != nil {
			return err
		}
		_, err = f.WriteString(def.String)
		f.Close()
		if err != nil {
			return err
		}
	}
	db.Close()
	log.Println("Found and saved defintions for", len(validIndices), "of", len(sprocNames), "active stored procedures")
	log.Println("Starting parsing phase (this can take a while)...")

	// initiate progress bar
	bar = pb.New(len(validIndices))
	bar.ShowFinalTime = true
	bar.ShowBar = true
	bar.SetMaxWidth(80)
	bar.Start()

	for _, i := range validIndices {
		var def []byte
		def, err = ioutil.ReadFile(filepath.Join(defDir, sprocNames[i]+".sql"))
		if err != nil {
			return err
		}
		outCh <- keyValue{key: sprocNames[i], value: string(def)}
	}
	return nil
}

func handleTables(ch <-chan []string, done chan<- bool) {
	f, err := os.Create(filepath.Join(outDir, "table_sources.csv"))
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.UseCRLF = true
	w.Write([]string{"Stored Procedure", "Table Used"})
	for row := range ch {
		w.Write(row)
	}
	w.Flush()
	done <- true
}

func handlePortfolios(ch <-chan []string, done chan<- bool) {
	f, err := os.Create(filepath.Join(outDir, "portfolio_codes.csv"))
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.UseCRLF = true
	w.Write([]string{"Stored Procedure", "Portfolio Code Mentioned"})
	for row := range ch {
		w.Write(row)
	}
	w.Flush()
	done <- true
}

func handleErrors(ch <-chan []string, done chan<- bool) {
	f, err := os.Create(filepath.Join(outDir, "parsing_errors.csv"))
	if err != nil {
		log.Fatalln(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.UseCRLF = true
	w.Write([]string{"Stored Procedure", "Error Count"})
	counts := make(map[string]int)
	for row := range ch {
		counts[row[0]]++
	}
	for proc, count := range counts {
		w.Write([]string{proc, strconv.Itoa(count)})
	}
	w.Flush()
	done <- true
}

func handleSprocDetails(defDir string, inCh <-chan keyValue, outCh chan<- []string, pCh chan<- []string, errCh chan<- []string, done *sync.WaitGroup) {
	for s := range inCh {
		f, err := os.Create(filepath.Join(defDir, s.key+".sql"))
		if err != nil {
			log.Fatalln(err)
		}
		_, err = f.WriteString(s.value)
		f.Close()
		if err != nil {
			log.Fatalln(err)
		}
		errors, tables, portfolios := parseSproc(s)
		for _, e := range errors {
			errCh <- []string{s.key, e}
		}
		for _, t := range tables {
			outCh <- []string{s.key, t}
		}
		for _, p := range portfolios {
			pCh <- []string{s.key, p}
		}
		bar.Increment()
	}
	done.Done()
}

func removeBrackets(in string) string {
	return strings.TrimPrefix(strings.TrimSuffix(in, "]"), "[")
}

func normalizeTableName(in string) (out string) {
	elems := strings.Split(strings.ToUpper(strings.TrimSpace(in)), ".")
	switch len(elems) {
	case 0:
		// nothin' here, send back empty string
		log.Fatalln("missing table name")
	case 1, 2:
		// assumption: it's just the table name or dbo.table_name
		out = removeBrackets(elems[len(elems)-1])
	case 3:
		var normalizedElems []string
		for _, elem := range elems {
			normalizedElems = append(normalizedElems, removeBrackets(elem))
		}
		if normalizedElems[0] == `BRS` {
			out = normalizedElems[2]
		} else {
			out = strings.Join(normalizedElems, ".")
		}
	default:
		log.Fatalln("unhandled table name format:", in)
	}
	return
}

func (l *errorListener) SyntaxError(recognizer antlr.Recognizer, offendingSymbol interface{}, line, column int, msg string, e antlr.RecognitionException) {
	l.errCh <- keyValue{key: l.sprocName, value: fmt.Sprintf("Line: %d, Column: %d, Error: %s", line, column, msg)}
}

func newErrorListener(ch chan<- keyValue, sprocName string) *errorListener {
	return &errorListener{
		antlr.NewDefaultErrorListener(),
		ch,
		sprocName,
	}
}

// parse sproc runs the dumped sproc definition through a parser generated by antlr
// using the TSQL grammar definition from https://github.com/antlr/grammars-v4/tree/master/tsql
// The TreeShapeListener definied in this package extends the default listener generated by antlr to capture the
// data we care about.  Similarly, the ErrorListener defined in this package receives and handles parsing errors.
// The caller specifies channels to receive a stream of tables used and errors encountered during parsing. The key of
// the sproc parameter is the (string) name of the stored procedure, and the value is the (string) text of the sproc
// defintion
func parseSproc(sproc keyValue) (errors, tables []string, portfolios []string) {
	tCh := make(chan string)
	pCh := make(chan string)
	eCh := make(chan keyValue)
	wg := new(sync.WaitGroup)
	wg.Add(3)
	go func(ch <-chan keyValue) {
		for err := range ch {
			errors = append(errors, err.value)
		}
		wg.Done()
	}(eCh)
	go func(ch <-chan string) {
		for table := range ch {
			tables = append(tables, table)
		}
		wg.Done()
	}(tCh)
	go func(ch <-chan string) {
		for pCode := range ch {
			portfolios = append(portfolios, pCode)
		}
		wg.Done()
	}(pCh)
	input := antlr.NewInputStream(sproc.value)
	lexer := parser.NewtsqlLexer(input)
	stream := antlr.NewCommonTokenStream(lexer, 0)
	p := parser.NewtsqlParser(stream)
	p.RemoveErrorListeners()
	errL := newErrorListener(eCh, sproc.key)
	p.AddErrorListener(errL)
	p.BuildParseTrees = true
	if faster {
		p.GetInterpreter().SetPredictionMode(antlr.PredictionModeSLL)
	}
	tree := p.Tsql_file()
	antlr.ParseTreeWalkerDefault.Walk(NewTreeShapeListener(tCh, pCh), tree)
	close(tCh)
	close(pCh)
	close(eCh)
	wg.Wait()
	return
}

// NewSprocInfo returns a data structure ready to record stored procedure metadata from a listener
func NewSprocInfo() *SprocInfo {
	return &SprocInfo{
		Tables:     make(map[string]bool),
		Aliases:    make(map[string]bool),
		Portfolios: make(map[string]bool),
	}
}

// NewTreeShapeListener returns an allocated TreeShapeListener
func NewTreeShapeListener(tablesCh, portfoliosCh chan<- string) *TreeShapeListener {
	return &TreeShapeListener{
		&parser.BasetsqlListener{},
		false,
		NewSprocInfo(),
		tablesCh,
		portfoliosCh,
	}
}

// EnterTable_name is called when the parser enters a `table_name` node,
// which includes the name of the table from whcih data is sourced
func (l *TreeShapeListener) EnterTable_name(ctx *parser.Table_nameContext) {
	n := normalizeTableName(strings.TrimSpace(ctx.GetText()))
	if len(n) > 0 {
		l.info.Tables[n] = true
	}
}

// EnterSimple_id is called when the parser enters a `simple_id` node
func (l *TreeShapeListener) EnterSimple_id(ctx *parser.Simple_idContext) {
	id := strings.TrimSpace(ctx.GetText())
	if portfolioKeys[id] {
		l.info.Portfolios[id] = true
	}
}

// EnterConstant is called when the parser enters a `simple_id` node
func (l *TreeShapeListener) EnterConstant(ctx *parser.ConstantContext) {
	id := strings.TrimSpace(ctx.GetText())
	id = strings.TrimPrefix(id, `'`)
	id = strings.TrimSuffix(id, `'`)
	if portfolioKeys[id] {
		l.info.Portfolios[id] = true
	}
	// handle suffix wildcards
	if strings.HasSuffix(id, "%") {
		id = strings.TrimSuffix(id, "%")
		for k := range portfolioKeys {
			if strings.HasPrefix(k, id) {
				l.info.Portfolios[k] = true
			}
		}
	}
	// handle prefix wildcards
	if strings.HasPrefix(id, "%") {
		id = strings.TrimPrefix(id, "%")
		for k := range portfolioKeys {
			if strings.HasSuffix(k, id) {
				l.info.Portfolios[k] = true
			}
		}
	}
}

// EnterTable_alias is called when the parser enters a `table_alias` node,
// which is pulled into a list of table references to ignore
func (l *TreeShapeListener) EnterTable_alias(ctx *parser.Table_aliasContext) {
	n := normalizeTableName(strings.TrimSpace(ctx.GetText()))
	if len(n) > 0 {
		l.info.Aliases[strings.ToUpper(n)] = true
	}
}

// ExitTsql_file is called when the parser reaches the end of the TSQL input,
// at which point the table names used are analyzed and sent down a channel
func (l *TreeShapeListener) ExitTsql_file(ctx *parser.Tsql_fileContext) {
	seen := make(map[string]bool)
	for table := range l.info.Tables {
		if strings.HasPrefix(table, "#") {
			continue
		}
		_, ok := l.info.Aliases[strings.ToUpper(table)]
		if ok {
			// skip it - it's an alias
			continue
		}
		_, ok = seen[strings.ToUpper(table)]
		if ok {
			// skip it - it's a dupe
			continue
		}
		seen[strings.ToUpper(table)] = true
		if strings.Contains(table, ".") {
			// no need to check the whitelist -- this table refers to a DB other than BRS
			l.tablesUsedCh <- table
			continue
		}

		// check to see if the table is in the whitelist populated during getSprocs()
		_, ok = whitelist[strings.ToUpper(table)]
		if !ok {
			// skip it -- it's not in the whitelist
			continue
		}
		l.tablesUsedCh <- table
	}
	for portfolioCode := range l.info.Portfolios {
		l.portfolioCodesCh <- portfolioCode
	}
}
