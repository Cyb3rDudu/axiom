# Database Troubleshooting

Quick fixes for PostgreSQL database issues.

## Connection Issues

### Database Won't Start

**Error:** Connection refused to PostgreSQL

**Solution:**
```bash
# Check if postgres is running
docker compose ps axiom-postgres

# Start postgres first
docker compose up -d postgres
sleep 10

# Then start other services
docker compose up -d

# Check logs if still failing
docker compose logs axiom-postgres
```

### Authentication Failed

**Error:** Password authentication failed

**Solution:**
```bash
# Check your .env file has correct credentials
grep POSTGRES .env

# Should show:
# POSTGRES_DB=axiom_db
# POSTGRES_USER=axiom_user
# POSTGRES_PASSWORD=axiom_password

# If wrong, fix and restart
docker compose down
docker compose up -d
```

### Database Not Initialized

**Error:** Database "axiom_db" does not exist

**Solution:**
```bash
# Let initialization complete
docker compose down
docker compose up -d postgres
sleep 30  # Wait for init scripts

# Check initialization logs
docker compose logs axiom-postgres | grep "database system is ready"

# Start other services
docker compose up -d
```

## Data Issues

### Corrupted Database

**Symptoms:** Crashes, errors, inconsistent data

**Solution - Complete Reset:**
```bash
# WARNING: This deletes ALL data!
docker compose down -v
docker compose up -d
```

### Disk Full

**Error:** Could not extend file, No space left

**Solution:**
```bash
# Check disk usage
df -h
docker system df

# Clean up Docker
docker system prune -a --volumes

# Check database size
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "\l+"
```

## Migration Issues

### Migration Failed

**Error:** Migration error on startup

**Solution:**
```bash
# Check migration status
docker compose logs axiom-backend | grep -i migration

# Re-run migrations
docker exec axiom-backend python -m database.init_postgres

# If that fails, reset database
docker compose down -v
docker compose up -d
```

## Performance Issues

### Slow Queries

**Symptoms:** Slow searches, timeouts

**Quick fixes:**
```bash
# Restart database
docker compose restart postgres

# Check active connections
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "
SELECT count(*) FROM pg_stat_activity;
"

# Kill long-running queries
docker exec axiom-postgres psql -U axiom_user -d axiom_db -c "
SELECT pg_terminate_backend(pid) 
FROM pg_stat_activity 
WHERE state = 'active' 
AND query_start < NOW() - INTERVAL '5 minutes';
"
```

## Backup and Recovery

### Quick Backup

```bash
# Backup database
docker exec axiom-postgres pg_dump -U axiom_user axiom_db > backup.sql

# Backup with timestamp
docker exec axiom-postgres pg_dump -U axiom_user axiom_db > backup_$(date +%Y%m%d_%H%M%S).sql
```

### Restore from Backup

```bash
# Stop services
docker compose down

# Start only postgres
docker compose up -d postgres
sleep 10

# Restore
docker exec -i axiom-postgres psql -U axiom_user axiom_db < backup.sql

# Start all services
docker compose up -d
```

## Common Fixes

### Restart Database

```bash
docker compose restart postgres
```

### Check Database Logs

```bash
docker compose logs axiom-postgres --tail=100
```

### Test Connection

```bash
# From host
docker exec axiom-postgres pg_isready

# From backend
docker exec axiom-backend python -c "
from database.database import get_db
db = next(get_db())
print('Connected!' if db else 'Failed')
"
```

### Reset Everything

⚠️ **WARNING: Deletes all data!**

```bash
docker compose down -v
docker compose up -d
```

## Document Consistency

### Check for Orphaned Data

```bash
# Run consistency check
docker exec axiom-backend python cli_document_consistency.py system-status

# Clean up orphans
docker exec axiom-backend python cli_document_consistency.py cleanup-all
```

## Still Having Issues?

1. Check logs: `docker compose logs axiom-postgres`
2. Verify .env settings
3. Try complete reset (WARNING: data loss)
4. See [Database Reset Guide](../database-reset.md)