-- Initialize the database for S3 document migration logging

USE s3_documents;

-- Create the document_logs table
CREATE TABLE IF NOT EXISTS document_logs (
    id INT AUTO_INCREMENT PRIMARY KEY,
    file_key VARCHAR(1000) NOT NULL,
    status ENUM('success', 'error', 'pending') NOT NULL,
    message TEXT,
    moved_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    
    -- Indexes for better performance
    INDEX idx_file_key (file_key(255)),
    INDEX idx_status (status),
    INDEX idx_moved_at (moved_at),
    INDEX idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create a table for tracking migration batches (optional)
CREATE TABLE IF NOT EXISTS migration_batches (
    id INT AUTO_INCREMENT PRIMARY KEY,
    batch_name VARCHAR(255) NOT NULL,
    total_files INT DEFAULT 0,
    successful_files INT DEFAULT 0,
    failed_files INT DEFAULT 0,
    started_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP NULL,
    status ENUM('running', 'completed', 'failed') DEFAULT 'running',
    
    UNIQUE KEY unique_batch_name (batch_name),
    INDEX idx_status (status),
    INDEX idx_started_at (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Create view for failed migrations
CREATE VIEW failed_migrations AS
SELECT 
    file_key,
    message,
    moved_at,
    created_at
FROM document_logs 
WHERE status = 'error'
ORDER BY created_at DESC;

-- Create view for recent migrations
CREATE VIEW recent_migrations AS
SELECT 
    file_key,
    status,
    message,
    moved_at
FROM document_logs 
WHERE moved_at >= DATE_SUB(NOW(), INTERVAL 24 HOUR)
ORDER BY moved_at DESC;

-- Grant permissions to application user
GRANT SELECT, INSERT, UPDATE ON s3_documents.* TO 's3user'@'%';

-- Flush privileges
FLUSH PRIVILEGES;
