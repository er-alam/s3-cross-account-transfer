#!/bin/bash

echo "🔍 Checking S3 Document Mover Setup..."
echo "=================================="

# Check if .env file exists and has real values
if [ ! -f ".env" ]; then
    echo "❌ .env file not found. Please copy from .env.example and update with real values."
    exit 1
fi

# Check for placeholder values
if grep -q "your_.*_here" .env; then
    echo "⚠️  Found placeholder values in .env file:"
    grep "your_.*_here" .env
    echo ""
    echo "Please update .env with your actual AWS credentials:"
    echo "1. SRC_ACCESS_KEY and SRC_SECRET_KEY (source S3 account)"
    echo "2. DST_ACCESS_KEY and DST_SECRET_KEY (destination S3 account)"
    echo "3. SRC_BUCKET and DST_BUCKET (actual bucket names)"
    echo "4. SRC_REGION and DST_REGION (AWS regions)"
    exit 1
fi

echo "✅ .env file exists with actual values"

# Check Docker services
echo ""
echo "🐳 Checking Docker services..."
if ! docker-compose ps | grep -q "Up.*healthy"; then
    echo "❌ MySQL service is not healthy. Starting services..."
    docker-compose up -d
    echo "⏳ Waiting for MySQL to be ready..."
    sleep 10
fi

echo "✅ Docker services are running"

# Test database connection
echo ""
echo "📊 Testing database connection..."
if docker-compose exec -T mysql mysql -u s3user -ps3password s3_documents -e "SELECT 1;" > /dev/null 2>&1; then
    echo "✅ Database connection successful"
else
    echo "❌ Database connection failed"
    exit 1
fi

echo ""
echo "🚀 Ready to test Go application!"
echo "Run: go run main.go"
