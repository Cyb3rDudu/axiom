# Database Reset and Management

Guide for managing AXIOM's PostgreSQL database, including reset, backup, and recovery procedures.

## Understanding AXIOM's Data Architecture

AXIOM uses PostgreSQL with pgvector extension for all data storage:

1. **PostgreSQL Database** (`axiom_db`)
    - User accounts and authentication
    - Documents metadata and records  
    - Document chunks with embeddings (pgvector)
    - Chat sessions and messages
    - Missions and execution logs
    - Writing sessions and drafts

2. **File Storage** (Docker volumes)
    - Original uploaded files
    - Processed markdown files
    - Model cache files

## Quick Database Reset

### Complete Reset (Recommended)

The simplest way to completely reset all data:

```bash
# Stop all services
docker compose down

# Remove all volumes (THIS DELETES ALL DATA!)
docker compose down -v

# Restart fresh
docker compose up -d --build
```

This will:

- Delete the PostgreSQL database
- Remove all stored files
- Clear model caches
- Start with a fresh, empty system

### Using axiom-cli.sh

The recommended way to reset the database:

```bash
# Check current database status
./axiom-cli.sh reset-db --stats

# Reset with backup
./axiom-cli.sh reset-db --backup

# Force reset without confirmation
./axiom-cli.sh reset-db --force
```

### Using Database Reset Script

```bash
# Copy reset script to container
docker cp reset_databases.py axiom-backend:/app/

# Run reset with options
docker exec -it axiom-backend python reset_databases.py --backup

# Remove script after use
docker exec axiom-backend rm /app/reset_databases.py
```

## Database Backup and Restore

### Full Database Backup

Create complete backup of PostgreSQL data:

```bash
# Create backup directory
mkdir -p backups/$(date +%Y%m%d)

# Backup PostgreSQL database
docker exec axiom-postgres pg_dump -U axiom_user axiom_db > backups/$(date +%Y%m%d)/axiom.sql

# Backup document files
tar czf backups/$(date +%Y%m%d)/documents.tar.gz axiom_backend/data/

# Backup model cache (optional, large)
tar czf backups/$(date +%Y%m%d)/models.tar.gz axiom_model_cache/
```

### Restore from Backup

```bash
# Stop services
docker compose down

# Start only PostgreSQL
docker compose up -d postgres

# Restore database
docker exec -i axiom-postgres psql -U axiom_user axiom_db < backups/20240101/axiom.sql

# Restore document files
tar xzf backups/20240101/documents.tar.gz

# Start all services
docker compose up -d
```

## Database Operations

### Check Database Health

```bash
# Check PostgreSQL status
docker exec axiom-postgres pg_isready -U axiom_user

# Connect to database
docker exec -it axiom-postgres psql -U axiom_user -d axiom_db

# List all tables
\dt

# Count records in main tables
SELECT 
  'users' as table_name, COUNT(*) as count FROM users
UNION ALL
  SELECT 'documents', COUNT(*) FROM documents
UNION ALL
  SELECT 'document_chunks', COUNT(*) FROM document_chunks
UNION ALL
  SELECT 'chats', COUNT(*) FROM chats;

# Exit psql
\q
```

### View Database Size

```bash
# Check database size
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "
  SELECT pg_database_size('axiom_db')/1024/1024 as size_mb;
"

# Check table sizes
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "
  SELECT 
    schemaname,
    tablename,
    pg_size_pretty(pg_total_relation_size(schemaname||'.'||tablename)) AS size
  FROM pg_tables 
  WHERE schemaname = 'public'
  ORDER BY pg_total_relation_size(schemaname||'.'||tablename) DESC;
"
```

## Recovery Procedures

### If Database Won't Start

```bash
# Check logs
docker compose logs axiom-postgres

# Try rebuilding
docker compose down
docker compose up -d postgres

# If still failing, reset PostgreSQL volume
docker volume rm axiom_postgres-data
docker compose up -d postgres
```

## Maintenance Tasks

### Vacuum Database

Reclaim storage and update statistics:

```bash
# Full vacuum (locks tables)
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "VACUUM FULL;"

# Analyze for query optimization
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "ANALYZE;"
```

### Reindex Tables

Improve query performance:

```bash
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "REINDEX DATABASE axiom_db;"
```

### Clean Up Old Data

Remove old processing jobs:

```bash
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "
  DELETE FROM document_processing_jobs 
  WHERE created_at < NOW() - INTERVAL '30 days'
  AND status IN ('completed', 'failed');
"
```

## Troubleshooting

### Connection Issues

```bash
# Test connection
docker exec axiom-backend python -c "
from database.database import engine
print('Connected!' if engine else 'Failed')
"

# Check connection string
docker exec axiom-backend env | grep DATABASE_URL
```

### Permission Issues

```bash
# Grant all permissions to axiom_user
docker exec axiom-postgres psql -U postgres -d axiom_db -c "
  GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO axiom_user;
  GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO axiom_user;
"
```

### Disk Space Issues

```bash
# Check Docker volumes
docker system df

# Clean up unused data
docker system prune -a --volumes
```

## Best Practices

1. **Regular Backups** - Schedule daily backups of the database
2. **Test Restores** - Periodically test backup restoration
3. **Monitor Size** - Track database growth

## Important Notes

- The PostgreSQL database is the single source of truth
- Document chunks and embeddings are stored directly in PostgreSQL with pgvector
- Always backup before major operations

## Next Steps

- [CLI Usage Guide](cli-commands.md)
- [Document Processing](../../user-guide/documents/overview.md)
- [Backup Automation](../configuration/environment-variables.md#backup)