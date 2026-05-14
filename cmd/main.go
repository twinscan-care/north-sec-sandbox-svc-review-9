package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"review-service/internal/service"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	_ "github.com/lib/pq"
	"github.com/spf13/viper"
)

// Config holds all configuration for the service.
type Config struct {
	Port                string `mapstructure:"port"`
	DBHost              string `mapstructure:"db_host"`
	DBPort              string `mapstructure:"db_port"`
	DBUser              string `mapstructure:"db_user"`
	DBPass              string `mapstructure:"db_pass"`
	DBName              string `mapstructure:"db_name"`
	RedisHost           string `mapstructure:"redis_host"`
	RedisPort           string `mapstructure:"redis_port"`
	DatabaseURL         string `mapstructure:"database_url"`
	CharacteristicsFile string `mapstructure:"characteristics_file"`
}

// main is the entry point for the application.
func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	createCharacteristicsFileIfNeeded(config.CharacteristicsFile)

	gin.SetMode(viper.GetString("gin_mode"))

	db, err := initDB(config)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func() { _ = db.Close() }()

	redisClient := initRedis(config)
	defer func() { _ = redisClient.Close() }()

	service.InitCommentFuncMap(config.CharacteristicsFile)

	reviewService := service.NewReviewService(db, redisClient)

	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// Add CORS middleware
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "healthy",
			"service":   "review-service",
			"timestamp": time.Now().Format(time.RFC3339),
		})
	})

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// API routes (Return JSON)
	api := router.Group("/api/reviews")
	{
		api.GET("", reviewService.GetReviews)
		api.GET("/:id", reviewService.GetReview)
		api.POST("", reviewService.CreateReviewAPI)
		api.PUT("/:id", reviewService.UpdateReview)
		api.DELETE("/:id", reviewService.DeleteReview)
		api.GET("/product/:productId", reviewService.GetReviewsByProduct)
		api.GET("/product/:productId/stats", reviewService.GetProductReviewStats)
		api.POST("/reset", reviewService.ResetReviews)
	}

	log.Printf("Review Service starting on port %s", config.Port)
	if err := router.Run(":" + config.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// createCharacteristicsFileIfNeeded creates the characteristics file if it does not exist.
func createCharacteristicsFileIfNeeded(filePath string) {
	// Check if the file already exists
	if _, err := os.Stat(filePath); err == nil {
		// File exists
		return
	} else if !os.IsNotExist(err) {
		// An error other than "not exist" occurred
		log.Fatalf("Error checking for characteristics file: %v", err)
	}

	// File does not exist, create it with default content
	log.Printf("Characteristics file not found at %s, creating with default content.", filePath)

	// Default content for the characteristics file
	content := []byte(`{
    "defaultColor": "black",
    "defaultSize": "6'",
    "defaultWeight": "12 pounds",
    "defaultSurface": "wood",
    "defaultMaterial": "cotton"
}`)

	// Write the content to the file
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		log.Fatalf("Failed to write characteristics file: %v", err)
	}

	log.Println("Successfully created characteristics file.")
}

// loadConfig loads configuration from environment variables with sensible defaults.

// loadConfig loads configuration from environment variables with sensible defaults.
func loadConfig() (*Config, error) {
	viper.SetDefault("port", "8080")
	viper.SetDefault("db_host", "localhost")
	viper.SetDefault("db_port", "5432")
	viper.SetDefault("db_user", "postgres")
	viper.SetDefault("db_pass", "postgres")
	viper.SetDefault("db_name", "product_catalog")
	viper.SetDefault("redis_host", "localhost")
	viper.SetDefault("redis_port", "6379")
	viper.SetDefault("database_url", "")
	viper.SetDefault("characteristics_file", "/mnt/characteristics.json")

	// Bind environment variables. PORT is read as is, others are prefixed with SVC_.
	_ = viper.BindEnv("port", "PORT")
	_ = viper.BindEnv("db_host", "SVC_DB_HOST")
	_ = viper.BindEnv("db_port", "SVC_DB_PORT")
	_ = viper.BindEnv("db_user", "SVC_DB_USER")
	_ = viper.BindEnv("db_pass", "SVC_DB_PASS")
	_ = viper.BindEnv("db_name", "SVC_DB_NAME")
	_ = viper.BindEnv("redis_host", "SVC_REDIS_HOST")
	_ = viper.BindEnv("redis_port", "SVC_REDIS_PORT")
	_ = viper.BindEnv("database_url", "SVC_DATABASE_URL")
	_ = viper.BindEnv("characteristics_file", "SVC_CHARACTERISTICS_FILE")

	if configFile := os.Getenv("CONFIG_FILE"); configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			log.Printf("Warning: could not read config file %s: %v", configFile, err)
		}
	}

	var config Config
	err := viper.Unmarshal(&config)
	if err != nil {
		return nil, fmt.Errorf("unable to decode into struct, %v", err)
	}

	return &config, nil
}

// initDB establishes a connection to the PostgreSQL database and configures the connection pool.
func initDB(config *Config) (*sql.DB, error) {
	var dsn string
	if config.DatabaseURL != "" {
		dsn = config.DatabaseURL
	} else {
		dsn = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			config.DBHost, config.DBPort, config.DBUser, config.DBPass, config.DBName)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, err
	}

	// Log connection details for debugging
	var currentDB, searchPath string
	_ = db.QueryRow("SELECT current_database()").Scan(&currentDB)
	_ = db.QueryRow("SHOW search_path").Scan(&searchPath)
	log.Printf("Connected to database: %s, search_path: %s", currentDB, searchPath)

	// Initialize database schema
	stmts := getInitStatements()
	log.Printf("Executing %d init statements...", len(stmts))
	for i, stmt := range stmts {
		preview := stmt
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		if _, err := db.Exec(stmt); err != nil {
			log.Printf("Init statement %d/%d FAILED: %v | %s", i+1, len(stmts), err, preview)
		} else {
			log.Printf("Init statement %d/%d OK | %s", i+1, len(stmts), preview)
		}
	}

	// Verify tables were created
	var tableCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('categories', 'products', 'reviews')").Scan(&tableCount)
	log.Printf("Verification: found %d/3 expected tables in public schema", tableCount)

	log.Println("Database connection established")
	return db, nil
}

// initRedis establishes a connection to the Redis server.
func initRedis(config *Config) *redis.Client {
	const redisCertPath = "/var/secrets/redis-cert.pem"

	var tlsCfg *tls.Config
	certPEM, err := os.ReadFile(redisCertPath)
	if err != nil {
		log.Printf("Warning: could not read Redis TLS certificate at %s: %v. Proceeding without TLS.", redisCertPath, err)
	} else {
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(certPEM) {
			log.Printf("Warning: failed to parse Redis TLS certificate at %s. Proceeding without TLS.", redisCertPath)
		} else {
			tlsCfg = &tls.Config{
				RootCAs: certPool,
			}
		}
	}

	redisURL := os.Getenv("REDIS_URL")
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("Warning: could not parse REDIS_URL: %v. Falling back to defaults.", err)
		opts = &redis.Options{
			Addr: "localhost:6379",
			DB:   0,
		}
	}

	// If we have a custom CA cert, merge it into the TLS config from ParseURL
	if tlsCfg != nil && opts.TLSConfig != nil {
		opts.TLSConfig.RootCAs = tlsCfg.RootCAs
	} else if tlsCfg != nil {
		opts.TLSConfig = tlsCfg
	}

	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		log.Printf("Failed to connect to Redis: %v", err)
	} else {
		log.Println("Redis connection established")
	}
	return rdb
}

func getInitStatements() []string {
	return []string{
		// 1. Enable uuid-ossp extension
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`,

		// 2. Create the categories table
		`CREATE TABLE IF NOT EXISTS categories (
			id UUID PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT,
			parent_id UUID,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			CONSTRAINT fk_categories_parent_id FOREIGN KEY (parent_id) REFERENCES categories(id) ON DELETE SET NULL
		)`,

		// 3. Create the products table
		`CREATE TABLE IF NOT EXISTS products (
			id UUID PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT,
			price NUMERIC(10, 2) NOT NULL,
			inventory INT NOT NULL,
			category_id UUID,
			image_url VARCHAR(255),
			sku VARCHAR(100),
			is_active BOOLEAN DEFAULT TRUE,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			CONSTRAINT fk_products_category_id FOREIGN KEY (category_id) REFERENCES categories(id) ON DELETE SET NULL
		)`,

		// 4. Create the reviews table
		`CREATE TABLE IF NOT EXISTS reviews (
			id UUID PRIMARY KEY,
			product_id UUID NOT NULL,
			rating INT NOT NULL CHECK (rating >= 1 AND rating <= 5),
			title VARCHAR(255),
			comment TEXT,
			is_verified BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			CONSTRAINT fk_reviews_product_id FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE
		)`,

		// 5. Create indexes
		`CREATE INDEX IF NOT EXISTS idx_products_category_id ON products(category_id)`,
		`CREATE INDEX IF NOT EXISTS idx_categories_parent_id ON categories(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reviews_product_id ON reviews(product_id)`,

		// 6. Seed mock data
		`DO $$
DECLARE
    cat_electronics uuid := 'a1b2c3d4-e5f6-7788-9900-aabbccddeeff';
    cat_laptops uuid := 'b2c3d4e5-f6a7-8899-0011-bbccddeeff00';
    cat_keyboards uuid := 'c3d4e5f6-a7b8-9900-1122-ccddeeff0011';
    cat_mice uuid := 'd4e5f6a7-b8c9-0011-2233-ddeeff001122';
    prod_laptop_1 uuid := 'e5f6a7b8-c9d0-1122-3344-eeff00112233';
    prod_laptop_2 uuid := 'f6a7b8c9-d0e1-2233-4455-ff0011223344';
    prod_laptop_3 uuid := 'a7b8c9d0-e1f2-3344-5566-001122334455';
    prod_keyboard_1 uuid := 'b8c9d0e1-f2a3-4455-6677-112233445566';
    prod_keyboard_2 uuid := 'c9d0e1f2-a3b4-5566-7788-223344556677';
    prod_mouse_1 uuid := 'd0e1f2a3-b4c5-6677-8899-334455667788';
    prod_mouse_2 uuid := 'e1f2a3b4-c5d6-7788-9900-445566778899';
BEGIN

INSERT INTO categories (id, name, description, parent_id) VALUES
(cat_electronics, 'Electronics', 'Gadgets and devices', NULL),
(cat_laptops, 'Laptops', 'Portable computers', cat_electronics),
(cat_keyboards, 'Keyboards', 'Mechanical and membrane keyboards', cat_electronics),
(cat_mice, 'Mice', 'Gaming and office mice', cat_electronics)
ON CONFLICT (id) DO NOTHING;

INSERT INTO products (id, name, description, price, inventory, category_id, image_url, sku) VALUES
(prod_laptop_1, 'ProBook 15', 'A powerful and sleek laptop for professionals.', 1299.99, 50, cat_laptops, 'https://images.unsplash.com/photo-1517336714731-489689fd1ca8?w=500', 'PRO-15-2024'),
(prod_laptop_2, 'GamerX Pro', 'Top-tier gaming laptop with RGB lighting.', 1999.99, 25, cat_laptops, 'https://images.unsplash.com/photo-1588872657578-7efd1f1555ed?w=500', 'GAMERX-PRO-24'),
(prod_laptop_3, 'UltraSlim Air', 'Lightweight and portable for on-the-go users.', 999.50, 100, cat_laptops, 'https://images.unsplash.com/photo-1496181133206-80ce9b88a853?w=500', 'USAIR-24-SL'),
(prod_keyboard_1, 'MechanoType K1', 'Clicky mechanical keyboard with customizable keys.', 150.00, 200, cat_keyboards, 'https://images.unsplash.com/photo-1595181330351-789390274718?w=500', 'MECH-K1-BLUE'),
(prod_keyboard_2, 'SilentKey S2', 'Quiet and comfortable keyboard for office use.', 75.50, 300, cat_keyboards, 'https://images.unsplash.com/photo-1587829741301-dc798b83add3?w=500', 'SILENT-S2-BLK'),
(prod_mouse_1, 'GamerPoint M-Pro', 'High-DPI gaming mouse with 8 programmable buttons.', 89.99, 150, cat_mice, 'https://images.unsplash.com/photo-1615663249854-d9b73de124a6?w=500', 'GP-MPRO-RGB'),
(prod_mouse_2, 'ErgoClick E-1', 'Ergonomic vertical mouse to reduce wrist strain.', 49.99, 250, cat_mice, 'https://images.unsplash.com/photo-1628375639392-142203901f41?w=500', 'ERGO-E1-GRY')
ON CONFLICT (id) DO NOTHING;

INSERT INTO reviews (id, product_id, rating, title, comment, is_verified) VALUES
(uuid_generate_v4(), prod_laptop_1, 5, 'Absolutely fantastic!', 'This laptop is fast, has a great screen, and the battery life is amazing.', true),
(uuid_generate_v4(), prod_laptop_1, 4, 'Very good, but not perfect', 'A solid choice for work, but it can get a bit hot under heavy load.', true),
(uuid_generate_v4(), prod_laptop_2, 5, 'Gaming Beast!', 'Runs every game I throw at it on ultra settings. The RGB is a nice touch.', true),
(uuid_generate_v4(), prod_keyboard_1, 5, 'Best keyboard ever', 'The clicky sound is so satisfying. My typing speed has actually improved.', false),
(uuid_generate_v4(), prod_keyboard_1, 4, 'A bit loud for the office', 'Great to type on, but my colleagues are not fans of the noise.', true),
(uuid_generate_v4(), prod_mouse_1, 5, 'Incredible for gaming', 'Super responsive and the customizable buttons are a game changer.', true),
(uuid_generate_v4(), prod_mouse_2, 5, 'My wrist thanks me', 'Took a day to get used to, but now I can''t go back to a regular mouse.', true)
ON CONFLICT (id) DO NOTHING;

END $$`,
	}
}

