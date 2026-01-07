package main

import (
"context"
"database/sql"
"encoding/json"
"fmt"
"log"
"net/http"
"os"
"time"

"github.com/go-redis/redis/v8"
"github.com/gorilla/mux"
_ "github.com/lib/pq"
)

type Entry struct {
ID        int       `json:"id"`
Name      string    `json:"name"`
Message   string    `json:"message"`
CreatedAt time.Time `json:"created_at"`
}

type App struct {
DB    *sql.DB
Redis *redis.Client
Ctx   context.Context
}

func main() {
app := &App{Ctx: context.Background()}

dbHost := getEnv("DB_HOST", "localhost")
dbPort := getEnv("DB_PORT", "5432")
dbUser := getEnv("DB_USER", "guestbook")
dbPass := getEnv("DB_PASSWORD", "password")
dbName := getEnv("DB_NAME", "guestbook")

dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
dbHost, dbPort, dbUser, dbPass, dbName)

var err error
app.DB, err = sql.Open("postgres", dsn)
if err != nil {
log.Fatal("Could not connect to database:", err)
}
defer app.DB.Close()

for i := 0; i < 30; i++ {
err = app.DB.Ping()
if err == nil {
break
}
log.Println("Waiting for database...")
time.Sleep(2 * time.Second)
}

if err != nil {
log.Fatal("Database not available:", err)
}

log.Println("✓ Connected to PostgreSQL")
app.initDB()

redisHost := getEnv("REDIS_HOST", "localhost")
redisPort := getEnv("REDIS_PORT", "6379")
redisPass := getEnv("REDIS_PASSWORD", "")

app.Redis = redis.NewClient(&redis.Options{
Addr:     fmt.Sprintf("%s:%s", redisHost, redisPort),
Password: redisPass,
DB:       0,
})

_, err = app.Redis.Ping(app.Ctx).Result()
if err != nil {
log.Println("⚠ Redis not available, continuing without cache:", err)
} else {
log.Println("✓ Connected to Redis")
}

r := mux.NewRouter()
r.Use(corsMiddleware)

r.HandleFunc("/health", app.healthHandler).Methods("GET")
r.HandleFunc("/api/entries", app.getEntriesHandler).Methods("GET")
r.HandleFunc("/api/entries", app.createEntryHandler).Methods("POST")
r.HandleFunc("/api/stats", app.statsHandler).Methods("GET")

port := getEnv("PORT", "8080")
log.Printf("🚀 Server starting on port %s", port)
log.Fatal(http.ListenAndServe(":"+port, r))
}

func (app *App) initDB() {
query := `
CREATE TABLE IF NOT EXISTS entries (
id SERIAL PRIMARY KEY,
name VARCHAR(100) NOT NULL,
message TEXT NOT NULL,
created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`

_, err := app.DB.Exec(query)
if err != nil {
log.Fatal("Could not create table:", err)
}
log.Println("✓ Database schema ready")
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
health := map[string]interface{}{
"status": "healthy",
"time":   time.Now(),
}

if err := app.DB.Ping(); err != nil {
health["database"] = "unhealthy"
health["status"] = "degraded"
} else {
health["database"] = "healthy"
}

if _, err := app.Redis.Ping(app.Ctx).Result(); err != nil {
health["redis"] = "unhealthy"
} else {
health["redis"] = "healthy"
}

json.NewEncoder(w).Encode(health)
}

func (app *App) getEntriesHandler(w http.ResponseWriter, r *http.Request) {
cacheKey := "entries:all"

if app.Redis != nil {
cached, err := app.Redis.Get(app.Ctx, cacheKey).Result()
if err == nil {
w.Header().Set("X-Cache", "HIT")
w.Header().Set("Content-Type", "application/json")
w.Write([]byte(cached))
return
}
}

rows, err := app.DB.Query(`
SELECT id, name, message, created_at 
FROM entries 
ORDER BY created_at DESC 
LIMIT 100
`)
if err != nil {
http.Error(w, err.Error(), http.StatusInternalServerError)
return
}
defer rows.Close()

entries := []Entry{}
for rows.Next() {
var e Entry
if err := rows.Scan(&e.ID, &e.Name, &e.Message, &e.CreatedAt); err != nil {
continue
}
entries = append(entries, e)
}

if app.Redis != nil {
jsonData, _ := json.Marshal(entries)
app.Redis.Set(app.Ctx, cacheKey, jsonData, 30*time.Second)
}

w.Header().Set("X-Cache", "MISS")
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(entries)
}

func (app *App) createEntryHandler(w http.ResponseWriter, r *http.Request) {
var entry Entry
if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
http.Error(w, "Invalid data", http.StatusBadRequest)
return
}

if entry.Name == "" || entry.Message == "" {
http.Error(w, "Name and message required", http.StatusBadRequest)
return
}

err := app.DB.QueryRow(`
INSERT INTO entries (name, message) 
VALUES ($1, $2) 
RETURNING id, created_at
`, entry.Name, entry.Message).Scan(&entry.ID, &entry.CreatedAt)

if err != nil {
http.Error(w, err.Error(), http.StatusInternalServerError)
return
}

if app.Redis != nil {
app.Redis.Del(app.Ctx, "entries:all")
app.Redis.Incr(app.Ctx, "stats:total_entries")
}

w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusCreated)
json.NewEncoder(w).Encode(entry)
}

func (app *App) statsHandler(w http.ResponseWriter, r *http.Request) {
stats := make(map[string]interface{})

var count int
app.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count)
stats["total_entries"] = count

w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(stats)
}

func corsMiddleware(next http.Handler) http.Handler {
return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Access-Control-Allow-Origin", "*")
w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

if r.Method == "OPTIONS" {
w.WriteHeader(http.StatusOK)
return
}

next.ServeHTTP(w, r)
})
}

func getEnv(key, defaultValue string) string {
if value := os.Getenv(key); value != "" {
return value
}
return defaultValue
}
