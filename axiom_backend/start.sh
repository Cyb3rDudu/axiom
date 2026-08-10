#!/bin/bash

# Startup script for AXIOM backend
# This script initializes the database and runs migrations before starting the FastAPI server

echo "🚀 Starting AXIOM Backend..."

# Wait for PostgreSQL to be ready
# NOTE: the probe MUST exit non-zero when the DB is not ready, otherwise the
# `&& break` fires on the first attempt regardless of success (review finding 3).
# init_postgres also retries internally with connect_with_retries, but this loop
# guards the very first reachability check before Python imports the engine.
echo "⏳ Waiting for PostgreSQL to be ready..."
db_ready=0
for i in {1..30}; do
    if python -c "
from database.database import test_connection
import sys
if test_connection():
    print('✅ PostgreSQL is ready!')
    sys.exit(0)
sys.exit(1)
"; then
        db_ready=1
        break
    fi
    echo "Waiting for PostgreSQL... ($i/30)"
    sleep 2
done

if [ "$db_ready" -ne 1 ]; then
    echo "❌ PostgreSQL did not become ready in time; aborting startup"
    exit 1
fi

# Initialize PostgreSQL database if needed. A failure here is now FATAL: we
# exit non-zero so systemd restarts the unit and retries the full init
# (previously init_postgres failures were swallowed as 'may be already
# initialized', which could boot the app with missing migration schema).
if [[ "$DATABASE_URL" == postgresql* ]]; then
    echo "🐘 Initializing PostgreSQL database..."
    if python -m database.init_postgres; then
        echo "✅ PostgreSQL initialization completed!"
    else
        echo "❌ PostgreSQL initialization FAILED (migrations/DB error); aborting startup"
        exit 1
    fi
fi

# Skip migrations - PostgreSQL schema is managed via SQL files
echo "📊 Skipping migrations (PostgreSQL schema managed via SQL files)"

# Start the FastAPI server
echo "🌐 Starting FastAPI server..."
# Convert LOG_LEVEL to lowercase for uvicorn
UVICORN_LOG_LEVEL=$(echo "${LOG_LEVEL:-error}" | tr '[:upper:]' '[:lower:]')
exec uvicorn main:app --host 0.0.0.0 --port 8000 --reload --log-level $UVICORN_LOG_LEVEL --timeout-keep-alive 1800 --timeout-graceful-shutdown 1800 