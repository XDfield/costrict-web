package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/gorm"
)

func main() {
	var (
		dsnFlag  string
		sqlStr   string
		filename string
		interact bool
		help     bool
	)

	flag.StringVar(&dsnFlag, "dsn", "", "PostgreSQL connection string (overrides config)")
	flag.StringVar(&sqlStr, "sql", "", "SQL statement to execute")
	flag.StringVar(&filename, "file", "", "SQL file to execute")
	flag.BoolVar(&interact, "i", false, "Interactive REPL mode")
	flag.BoolVar(&help, "h", false, "Show help")
	flag.BoolVar(&help, "help", false, "Show help")
	flag.Parse()

	if help {
		fmt.Println("sqlexec - PostgreSQL query tool for costrict-web")
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  sqlexec [options]")
		fmt.Println()
		fmt.Println("Options:")
		fmt.Println("  -dsn string     PostgreSQL connection string (overrides .env / env vars)")
		fmt.Println("  -sql string     Execute a single SQL statement")
		fmt.Println("  -file string    Execute SQL from a file")
		fmt.Println("  -i              Interactive REPL mode")
		fmt.Println("  -h, --help      Show this help")
		fmt.Println()
		fmt.Println("Connection priority: -dsn flag > DATABASE_URL env > .env file")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println(`  sqlexec -sql "SELECT device_id, status FROM devices"`)
		fmt.Println("  sqlexec -file ./debug/query.sql")
		fmt.Println("  sqlexec -i")
		return
	}

	cfg := config.Load()
	dsn := dsnFlag
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}
	if dsn == "" {
		log.Fatal("no connection string. Use -dsn, set DATABASE_URL, or configure .env")
	}

	db, err := database.Initialize(dsn)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}

	var dbName string
	rawDB, dbErr := db.DB()
	if dbErr == nil {
		if err := rawDB.QueryRow("SELECT current_database()").Scan(&dbName); err != nil {
			dbName = "?"
		}
	}
	fmt.Printf("Connected to: %s\n\n", dbName)

	switch {
	case filename != "":
		if err := execFile(db, filename); err != nil {
			log.Fatalf("file execution failed: %v", err)
		}
	case sqlStr != "":
		if err := execQuery(db, sqlStr); err != nil {
			log.Fatalf("SQL failed: %v", err)
		}
	case interact:
		execInteractive(db)
	default:
		fmt.Println("Error: specify -sql, -file, or -i")
		flag.Usage()
		os.Exit(1)
	}
}

func execQuery(db *gorm.DB, sqlStr string) error {
	sqlStr = strings.TrimSpace(sqlStr)
	if sqlStr == "" {
		return nil
	}
	sqlStr = strings.TrimSuffix(sqlStr, ";")

	upper := strings.ToUpper(sqlStr)
	isQuery := strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "SHOW") ||
		strings.HasPrefix(upper, "DESCRIBE") ||
		strings.HasPrefix(upper, "DESC") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "WITH")

	if isQuery {
		rows, err := db.Raw(sqlStr).Rows()
		if err != nil {
			return fmt.Errorf("query failed: %w", err)
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return fmt.Errorf("get columns failed: %w", err)
		}

		colW := make([]int, len(cols))
		for i, c := range cols {
			w := len(c) + 2
			if w < 30 {
				w = 30
			}
			colW[i] = w
		}

		printRow(cols, colW)
		printSep(colW)

		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range cols {
			ptrs[i] = &vals[i]
		}

		count := 0
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return fmt.Errorf("scan failed: %w", err)
			}
			row := make([]string, len(cols))
			for i, v := range vals {
				if v == nil {
					row[i] = "NULL"
				} else {
					switch val := v.(type) {
					case []byte:
						s := string(val)
						if len(s) > 60 {
							s = s[:57] + "..."
						}
						row[i] = s
					case string:
						if len(val) > 60 {
							val = val[:57] + "..."
						}
						row[i] = val
					default:
						s := fmt.Sprintf("%v", val)
						if len(s) > 60 {
							s = s[:57] + "..."
						}
						row[i] = s
					}
				}
				if len(row[i]) > colW[i] {
					colW[i] = len(row[i]) + 2
				}
			}
			printRow(row, colW)
			count++
		}

		fmt.Printf("\n%d row(s)\n", count)
		return nil
	}

	result := db.Exec(sqlStr)
	if result.Error != nil {
		return fmt.Errorf("exec failed: %w", result.Error)
	}
	fmt.Printf("OK, %d row(s) affected\n", result.RowsAffected)
	return nil
}

func printRow(cols []string, widths []int) {
	for i, c := range cols {
		if i > 0 {
			fmt.Print("  ")
		}
		fmt.Printf("%-*s", widths[i], c)
	}
	fmt.Println()
}

func printSep(widths []int) {
	for i, w := range widths {
		if i > 0 {
			fmt.Print("  ")
		}
		fmt.Printf("%s", strings.Repeat("-", w))
	}
	fmt.Println()
}

func execFile(db *gorm.DB, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var buf strings.Builder
	lineNum := 0

	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "#") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString(" ")
		if strings.HasSuffix(line, ";") {
			sqlStr := strings.TrimSpace(buf.String())
			buf.Reset()
			if sqlStr == "" {
				continue
			}
			fmt.Printf("\n--- SQL (line %d) ---\n", lineNum)
			if err := execQuery(db, sqlStr); err != nil {
				fmt.Printf("ERROR at line %d: %v\n", lineNum, err)
				return err
			}
		}
	}

	if buf.Len() > 0 {
		sqlStr := strings.TrimSpace(buf.String())
		if sqlStr != "" {
			fmt.Printf("\n--- SQL (end of file) ---\n")
			if err := execQuery(db, sqlStr); err != nil {
				return err
			}
		}
	}

	return sc.Err()
}

func execInteractive(db *gorm.DB) {
	prompt := "sqlexec> "
	fmt.Print(prompt)

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var buf strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		buf.WriteString(line)
		buf.WriteString(" ")

		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			sqlStr := strings.TrimSpace(buf.String())
			buf.Reset()
			if err := execQuery(db, sqlStr); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			}
			fmt.Print(prompt)
		} else if strings.HasSuffix(strings.TrimSpace(line), "\\") {
			fmt.Print("  -> ")
		}
	}

	if buf.Len() > 0 {
		sqlStr := strings.TrimSpace(buf.String())
		if sqlStr != "" {
			_ = execQuery(db, sqlStr)
		}
	}
}
