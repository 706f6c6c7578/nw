package main

import (
    "bufio"
    "crypto/tls"
    "encoding/json"
    "flag"
    "fmt"
    "net"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    "golang.org/x/net/proxy"
)

type GroupState struct {
    LastArticle int       `json:"last_article"`
    LastFetch   time.Time `json:"last_fetch"`
}

var state = make(map[string]GroupState)

func printUsage() {
    fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\nOptions:\n", os.Args[0])
    flag.PrintDefaults()
    fmt.Fprintf(os.Stderr, "\nExample:\n  %s -group alt.test -days 1 -latest\n", os.Args[0])
}

func getStateFileName(group string) string {
    return fmt.Sprintf("%s.json", group)
}

func loadState(group string) error {
    stateFileName := getStateFileName(group)
    path := filepath.Join(".", stateFileName) // Load from current directory
    data, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            // No state file exists yet, which is fine
            return nil
        }
        return fmt.Errorf("failed to read state file: %v", err)
    }

    return json.Unmarshal(data, &state)
}

func saveState(group string) error {
    stateFileName := getStateFileName(group)
    path := filepath.Join(".", stateFileName) // Save in current directory
    data, err := json.MarshalIndent(state, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to serialize state: %v", err)
    }

    if err := os.WriteFile(path, data, 0600); err != nil {
        return fmt.Errorf("failed to write state file: %v", err)
    }

    return nil
}

func dialNNTP(server string, port int, useTLS bool, proxyAddr string) (net.Conn, error) {
    address := fmt.Sprintf("%s:%d", server, port)

    if proxyAddr != "" {
        dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
        if err != nil {
            return nil, fmt.Errorf("proxy connection failed: %v", err)
        }
        conn, err := dialer.Dial("tcp", address)
        if err != nil {
            return nil, fmt.Errorf("proxy dial failed: %v", err)
        }

        if useTLS {
            return tls.Client(conn, &tls.Config{
                InsecureSkipVerify: true,
                ServerName:         server,
            }), nil
        }
        return conn, nil
    }

    if useTLS {
        return tls.Dial("tcp", address, &tls.Config{
            InsecureSkipVerify: true,
        })
    }
    return net.Dial("tcp", address)
}

func authenticateNNTP(conn net.Conn, username, password string) error {
    fmt.Fprintf(conn, "AUTHINFO USER %s\r\n", username)
    response, err := bufio.NewReader(conn).ReadString('\n')
    if err != nil {
        return fmt.Errorf("auth user read failed: %v", err)
    }
    if !strings.HasPrefix(response, "381") {
        return fmt.Errorf("auth user failed: %s", strings.TrimSpace(response))
    }

    fmt.Fprintf(conn, "AUTHINFO PASS %s\r\n", password)
    response, err = bufio.NewReader(conn).ReadString('\n')
    if err != nil {
        return fmt.Errorf("auth pass read failed: %v", err)
    }
    if !strings.HasPrefix(response, "281") {
        return fmt.Errorf("auth pass failed: %s", strings.TrimSpace(response))
    }
    return nil
}

func parseDate(dateStr string) (time.Time, error) {
    dateStr = strings.TrimSpace(dateStr)
    if idx := strings.Index(dateStr, " ("); idx > 0 {
        dateStr = dateStr[:idx]
    }
    return time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", dateStr)
}

func getRecentArticles(conn net.Conn, group string, days int, useLatest bool, maxBatchSize int) ([]string, error) {
    reader := bufio.NewReader(conn)

    fmt.Fprintf(conn, "GROUP %s\r\n", group)
    response, err := reader.ReadString('\n')
    if err != nil {
        return nil, fmt.Errorf("group command failed: %v", err)
    }
    if !strings.HasPrefix(response, "211 ") {
        return nil, fmt.Errorf("group selection failed: %s", strings.TrimSpace(response))
    }

    parts := strings.Fields(response)
    if len(parts) < 4 {
        return nil, fmt.Errorf("invalid group response: %s", strings.TrimSpace(response))
    }

    first, err := strconv.Atoi(parts[2])
    if err != nil {
        return nil, fmt.Errorf("invalid first article: %v", err)
    }
    last, err := strconv.Atoi(parts[3])
    if err != nil {
        return nil, fmt.Errorf("invalid last article: %v", err)
    }

    if useLatest {
        if saved, exists := state[group]; exists {
            if saved.LastArticle >= last {
                fmt.Fprintf(os.Stderr, "No new articles available since last fetch (last article: %d)\n", saved.LastArticle)
                return []string{}, nil // No new articles to fetch
            }
            if saved.LastArticle >= first && saved.LastArticle < last {
                first = saved.LastArticle + 1
                fmt.Fprintf(os.Stderr, "Resuming from article %d (last fetched was %d)\n", first, saved.LastArticle)
            }
        }
    }

    var articles []string
    cutoff := time.Now().AddDate(0, 0, -days)
    batchStart := first

    for batchStart <= last {
        batchEnd := batchStart + maxBatchSize - 1
        if batchEnd > last {
            batchEnd = last
        }

        fmt.Fprintf(conn, "XOVER %d-%d\r\n", batchStart, batchEnd)
        xoverHeader, err := reader.ReadString('\n')
        if err != nil {
            return nil, fmt.Errorf("xover header read failed: %v", err)
        }
        if !strings.HasPrefix(xoverHeader, "224 ") {
            return nil, fmt.Errorf("xover command failed: %s", strings.TrimSpace(xoverHeader))
        }

        var articleNumbers []string
        for {
            line, err := reader.ReadString('\n')
            if err != nil {
                return nil, fmt.Errorf("xover read failed: %v", err)
            }
            if line == ".\r\n" {
                break
            }

            fields := strings.Split(line, "\t")
            if len(fields) < 8 {
                continue
            }

            if days > 0 {
                date, err := parseDate(fields[3])
                if err != nil || date.Before(cutoff) {
                    continue
                }
            }
            articleNumbers = append(articleNumbers, fields[0])
        }

        for _, num := range articleNumbers {
            article, err := fetchArticle(conn, reader, num)
            if err != nil {
                fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
                continue
            }
            articles = append(articles, article)
        }

        if useLatest && batchEnd > first {
            state[group] = GroupState{
                LastArticle: batchEnd,
                LastFetch:   time.Now(),
            }
            if err := saveState(group); err != nil {
                fmt.Fprintf(os.Stderr, "Warning: failed to save state: %v\n", err)
            }
        }

        batchStart = batchEnd + 1
    }

    if len(articles) == 0 {
        return nil, fmt.Errorf("no articles found matching criteria")
    }

    return articles, nil
}

func fetchArticle(conn net.Conn, reader *bufio.Reader, articleNum string) (string, error) {
    fmt.Fprintf(conn, "ARTICLE %s\r\n", articleNum)
    response, err := reader.ReadString('\n')
    if err != nil {
        return "", fmt.Errorf("failed to fetch article %s: %v", articleNum, err)
    }
    if !strings.HasPrefix(response, "220 ") {
        return "", fmt.Errorf("article %s unavailable: %s", articleNum, strings.TrimSpace(response))
    }

    var article strings.Builder
    article.WriteString(response)

    for {
        line, err := reader.ReadString('\n')
        if err != nil {
            return "", fmt.Errorf("error reading article %s: %v", articleNum, err)
        }
        if line == ".\r\n" {
            break
        }
        if strings.HasPrefix(line, "..") {
            line = line[1:] // Unescape lines starting with ".."
        }
        article.WriteString(line)
    }

    // Append a single dot on its own line to mark the end of the article
    article.WriteString(".\r\n")

    return article.String(), nil
}

func main() {
    server := flag.String("server", "news.tcpreset.net", "NNTP server address")
    port := flag.Int("port", 119, "NNTP server port")
    group := flag.String("group", "alt.anonymous.messages", "Newsgroup to download from")
    days := flag.Int("days", 1, "Download articles from last N days (0 for all)")
    username := flag.String("user", "", "NNTP username")
    password := flag.String("pass", "", "NNTP password")
    useTLS := flag.Bool("tls", false, "Use TLS connection")
    proxyAddr := flag.String("proxy", "", "SOCKS proxy (e.g., 127.0.0.1:9050)")
    latest := flag.Bool("latest", false, "Only fetch articles newer than last run")
    maxBatchSize := flag.Int("batch", 500, "Maximum batch size for XOVER command")
    readTimeout := flag.Int("timeout", 1200, "Read timeout in seconds")
    flag.Parse()
    
    if err := loadState(*group); err != nil {
        fmt.Fprintf(os.Stderr, "Warning: failed to load state: %v\n", err)
    }

    conn, err := dialNNTP(*server, *port, *useTLS, *proxyAddr)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
        os.Exit(1)
    }
    defer conn.Close()

    if err := conn.SetReadDeadline(time.Now().Add(time.Duration(*readTimeout) * time.Second)); err != nil {
        fmt.Fprintf(os.Stderr, "Warning: couldn't set timeout: %v\n", err)
    }

    reader := bufio.NewReader(conn)
    if _, err := reader.ReadString('\n'); err != nil {
        fmt.Fprintf(os.Stderr, "Server greeting failed: %v\n", err)
        os.Exit(1)
    }

    if *username != "" {
        if err := authenticateNNTP(conn, *username, *password); err != nil {
            fmt.Fprintf(os.Stderr, "Authentication failed: %v\n", err)
            os.Exit(1)
        }
    }

    articles, err := getRecentArticles(conn, *group, *days, *latest, *maxBatchSize)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        os.Exit(1)
    }

    for _, article := range articles {
        fmt.Print(article)
    }

    fmt.Fprintf(conn, "QUIT\r\n")
}