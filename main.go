package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

type Job struct {
	Key string
}

type TransferStats struct {
	StartTime      time.Time
	EndTime        time.Time
	TotalFiles     int64
	SuccessCount   int64
	ErrorCount     int64
	TotalSizeBytes int64
	Method         map[string]int64
}

func main() {
	stats := &TransferStats{
		StartTime: time.Now(),
		Method:    make(map[string]int64),
	}

	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	if err := os.MkdirAll("logs", 0755); err != nil {
		log.Fatalf("Failed to create logs directory: %v", err)
	}

	db := connectDB()
	defer db.Close()

	if err := testDBConnection(db); err != nil {
		log.Fatalf("Database connection test failed: %v", err)
	}
	fmt.Println("✅ Database connected successfully.")

	srcS3 := initS3Client("SRC_ACCESS_KEY", "SRC_SECRET_KEY", "SRC_REGION")
	dstS3 := initS3Client("DST_ACCESS_KEY", "DST_SECRET_KEY", "DST_REGION")

	bucketSrc := os.Getenv("SRC_BUCKET")
	bucketDst := os.Getenv("DST_BUCKET")

	ctx := context.Background()
	if err := testS3Connection(ctx, srcS3, bucketSrc, "source"); err != nil {
		log.Fatalf("Source S3 connection test failed: %v", err)
	}
	fmt.Println("✅ Source S3 connected successfully.")

	if err := testS3Connection(ctx, dstS3, bucketDst, "destination"); err != nil {
		log.Fatalf("Destination S3 connection test failed: %v", err)
	}
	fmt.Println("✅ Destination S3 connected successfully.")

	keys := listKeys(ctx, srcS3, bucketSrc)
	stats.TotalFiles = int64(len(keys))
	fmt.Printf("📁 Found %d files in source bucket '%s'\n", len(keys), bucketSrc)

	if len(keys) == 0 {
		fmt.Println("⚠️  No files found in source bucket. Nothing to move.")
		stats.EndTime = time.Now()
		writeSummaryLog(stats, bucketSrc, bucketDst, 0)
		return
	}

	fmt.Printf("🚀 Starting transfer at: %s\n", stats.StartTime.Format("2006-01-02 15:04:05"))

	// Use a smaller buffered channel to avoid excessive memory usage
	jobChan := make(chan Job, 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex

	// Use a reasonable number of workers for large file counts
	workerCount := 150
	if len(keys) < 1000 {
		workerCount = 25
	}

	fmt.Printf("🔧 Starting %d workers for %d files\n", workerCount, len(keys))

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go worker(ctx, srcS3, dstS3, bucketSrc, bucketDst, db, jobChan, &wg, stats, &mu)
	}

	for _, key := range keys {
		jobChan <- Job{Key: key}
	}
	close(jobChan)
	wg.Wait()

	stats.EndTime = time.Now()
	duration := stats.EndTime.Sub(stats.StartTime)

	fmt.Printf("🏁 Transfer completed at: %s\n", stats.EndTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("⏱️  Total duration: %v\n", duration)
	fmt.Printf("📊 Summary: %d total, %d success, %d errors\n", stats.TotalFiles, stats.SuccessCount, stats.ErrorCount)

	if len(stats.Method) > 0 {
		fmt.Printf("📈 Methods used:\n")
		for method, count := range stats.Method {
			fmt.Printf("   - %s: %d files\n", method, count)
		}
	}

	writeSummaryLog(stats, bucketSrc, bucketDst, workerCount)
	fmt.Println("📄 Detailed summary written to logs/transfer_summary_[timestamp].log")
	fmt.Println("All files processed.")
}

func connectDB() *sql.DB {
	dsn := os.Getenv("MYSQL_DSN")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("DB connection error: %v", err)
	}
	return db
}

func initS3Client(accessKeyEnv, secretKeyEnv, regionEnv string) *s3.Client {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(os.Getenv(regionEnv)),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(os.Getenv(accessKeyEnv), os.Getenv(secretKeyEnv), ""),
		),
	)
	if err != nil {
		log.Fatalf("Unable to load AWS config: %v", err)
	}
	return s3.NewFromConfig(cfg)
}

func listKeys(ctx context.Context, client *s3.Client, bucket string) []string {
	var keys []string
	var token *string
	prefix := os.Getenv("SRC_PREFIX")

	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		}

		if prefix != "" {
			input.Prefix = aws.String(prefix)
			fmt.Printf("🔍 Filtering files with prefix: %s\n", prefix)
		}

		resp, err := client.ListObjectsV2(ctx, input)
		if err != nil {
			log.Fatalf("Unable to list objects: %v", err)
		}
		for _, obj := range resp.Contents {
			keys = append(keys, *obj.Key)
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return keys
}

func worker(ctx context.Context, src, dst *s3.Client, srcBucket, dstBucket string, db *sql.DB, jobs <-chan Job, wg *sync.WaitGroup, stats *TransferStats, mu *sync.Mutex) {
	defer wg.Done()
	for job := range jobs {
		fileStartTime := time.Now()
		err := moveObject(ctx, src, dst, srcBucket, dstBucket, job.Key, stats, mu)

		status := "success"
		msg := "moved"

		mu.Lock()
		if err != nil {
			status = "error"
			msg = err.Error()
			stats.ErrorCount++
		} else {
			stats.SuccessCount++
		}
		mu.Unlock()

		duration := time.Since(fileStartTime)
		logToDB(db, job.Key, status, msg)

		if err != nil {
			fmt.Printf("❌ Failed: %s (took %v) - %s\n", job.Key, duration, err.Error())
		}

	}
}

func moveObject(ctx context.Context, src, dst *s3.Client, srcBucket, dstBucket, key string, stats *TransferStats, mu *sync.Mutex) error {

	copySource := fmt.Sprintf("%s/%s", srcBucket, key)

	_, err := src.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(dstBucket),
		Key:               aws.String(key),
		CopySource:        aws.String(copySource),
		MetadataDirective: "COPY",
		StorageClass:      "STANDARD",
	})
	if err != nil {
		fmt.Printf("⚠️  Server-side copy failed, falling back to download/upload method\n")
		return moveObjectFallback(ctx, src, dst, srcBucket, dstBucket, key, stats, mu)
	}

	mu.Lock()
	stats.Method["server-side"]++
	mu.Unlock()

	return nil
}

func moveObjectFallback(ctx context.Context, src, dst *s3.Client, srcBucket, dstBucket, key string, stats *TransferStats, mu *sync.Mutex) error {
	fmt.Printf("🌊 Streaming: %s from %s to %s (no local storage)\n", key, srcBucket, dstBucket)

	headObj, err := src.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(srcBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("head object error: %w", err)
	}

	fileSize := *headObj.ContentLength
	fmt.Printf("📊 File size: %d bytes (%.2f GB)\n", fileSize, float64(fileSize)/(1024*1024*1024))

	mu.Lock()
	stats.TotalSizeBytes += fileSize
	mu.Unlock()

	if fileSize > 5*1024*1024*1024 {
		return fmt.Errorf("file too large (%d bytes / %.2f GB) - exceeds 5GB single PUT limit. Please use AWS CLI 'aws s3 cp' for files >5GB", fileSize, float64(fileSize)/(1024*1024*1024))
	}

	obj, err := src.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(srcBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("get error: %w", err)
	}
	defer obj.Body.Close()

	fmt.Printf("📤 Streaming upload: %s to %s (%.2f MB)\n", key, dstBucket, float64(fileSize)/(1024*1024))

	_, err = dst.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(dstBucket),
		Key:           aws.String(key),
		Body:          obj.Body,
		ContentLength: headObj.ContentLength,
		ContentType:   headObj.ContentType,
		Metadata:      headObj.Metadata,
	})
	if err != nil {
		return fmt.Errorf("streaming put error: %w", err)
	}

	mu.Lock()
	stats.Method["streaming"]++
	mu.Unlock()

	fmt.Printf("✅ Successfully streamed: %s (%.2f MB - no local storage)\n", key, float64(fileSize)/(1024*1024))
	return nil
}

func writeSummaryLog(stats *TransferStats, srcBucket, dstBucket string, workerCount int) {
	timestamp := stats.StartTime.Format("20060102_150405")
	filename := filepath.Join("logs", fmt.Sprintf("transfer_summary_%s.log", timestamp))

	duration := stats.EndTime.Sub(stats.StartTime)
	var filesPerSecond float64
	var mbPerSecond float64

	if duration.Seconds() > 0 {
		filesPerSecond = float64(stats.SuccessCount) / duration.Seconds()
		mbPerSecond = float64(stats.TotalSizeBytes) / (1024 * 1024) / duration.Seconds()
	}

	var successRate float64
	if stats.TotalFiles > 0 {
		successRate = float64(stats.SuccessCount) / float64(stats.TotalFiles) * 100
	}

	content := fmt.Sprintf(`S3 TRANSFER SUMMARY REPORT
=====================================

TRANSFER DETAILS:
- Start Time: %s
- End Time: %s
- Duration: %v
- Source Bucket: %s
- Destination Bucket: %s

FILE STATISTICS:
- Total Files Found: %d
- Successfully Transferred: %d
- Failed Transfers: %d
- Success Rate: %.2f%%

PERFORMANCE METRICS:
- Total Data Transferred: %.2f MB (%.2f GB)
- Average Speed: %.2f files/second
- Data Transfer Rate: %.2f MB/second

TRANSFER METHODS:
`,
		stats.StartTime.Format("2006-01-02 15:04:05"),
		stats.EndTime.Format("2006-01-02 15:04:05"),
		duration,
		srcBucket,
		dstBucket,
		stats.TotalFiles,
		stats.SuccessCount,
		stats.ErrorCount,
		successRate,
		float64(stats.TotalSizeBytes)/(1024*1024),
		float64(stats.TotalSizeBytes)/(1024*1024*1024),
		filesPerSecond,
		mbPerSecond,
	)

	for method, count := range stats.Method {
		percentage := float64(count) / float64(stats.SuccessCount) * 100
		content += fmt.Sprintf("- %s: %d files (%.1f%%)\n", method, count, percentage)
	}

	content += fmt.Sprintf(`
SYSTEM INFORMATION:
- Worker Threads: %d
- Storage Method: Zero local storage (direct S3-to-S3 transfer)
- Timestamp: %s

=====================================
Report generated by S3 Transfer Tool
`, workerCount, time.Now().Format("2006-01-02 15:04:05 MST"))

	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		log.Printf("Failed to write summary log: %v", err)
		return
	}

	fmt.Printf("📊 Transfer summary written to: %s\n", filename)
}

func logToDB(db *sql.DB, key, status, msg string) {
	_, err := db.Exec(`INSERT INTO document_logs (file_key, status, message, moved_at) VALUES (?, ?, ?, ?)`,
		key, status, msg, time.Now())
	if err != nil {
		log.Printf("DB insert failed for key %s: %v", key, err)
	}
}

func testDBConnection(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}

	var version string
	err := db.QueryRow("SELECT VERSION()").Scan(&version)
	if err != nil {
		return fmt.Errorf("version query failed: %w", err)
	}

	var tableExists int
	err = db.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = 's3_documents'
		AND table_name = 'document_logs'
	`).Scan(&tableExists)
	if err != nil {
		return fmt.Errorf("table check failed: %w", err)
	}

	if tableExists == 0 {
		return fmt.Errorf("document_logs table does not exist")
	}

	fmt.Printf("📊 Database version: %s\n", version)
	fmt.Printf("📋 document_logs table exists: ✅\n")
	return nil
}

func testS3Connection(ctx context.Context, client *s3.Client, bucket, connType string) error {
	if bucket == "" {
		return fmt.Errorf("%s bucket name is empty", connType)
	}

	_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("bucket access test failed for %s bucket '%s': %w", connType, bucket, err)
	}

	location, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		fmt.Printf("⚠️  Could not get %s bucket location: %v\n", connType, err)
	} else {
		region := "us-east-1"
		if location.LocationConstraint != "" {
			region = string(location.LocationConstraint)
		}
		fmt.Printf("🌍 %s bucket '%s' region: %s\n", connType, bucket, region)
	}

	return nil
}
