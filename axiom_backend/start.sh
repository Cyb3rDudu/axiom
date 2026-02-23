#!/bin/bash

# Startup script for AXIOM backend
# This script initializes the database and runs migrations before starting the FastAPI server

echo "🚀 Starting AXIOM Backend..."

# Wait for PostgreSQL to be ready
echo "⏳ Waiting for PostgreSQL to be ready..."
for i in {1..30}; do
    python -c "
from database.database import test_connection
if test_connection():
    print('✅ PostgreSQL is ready!')
    exit(0)
" && break
    echo "Waiting for PostgreSQL... ($i/30)"
    sleep 2
done

# Initialize PostgreSQL database if needed
if [[ "$DATABASE_URL" == postgresql* ]]; then
    echo "🐘 Initializing PostgreSQL database..."
    python -m database.init_postgres
    
    if [ $? -eq 0 ]; then
        echo "✅ PostgreSQL initialization completed!"
    else
        echo "⚠️  PostgreSQL initialization had issues (may be already initialized)"
    fi
fi

# Skip migrations - PostgreSQL schema is managed via SQL files
echo "📊 Skipping migrations (PostgreSQL schema managed via SQL files)"

# Start the FastAPI server
echo "🌐 Starting FastAPI server..."
# Convert LOG_LEVEL to lowercase for uvicorn
UVICORN_LOG_LEVEL=$(echo "${LOG_LEVEL:-error}" | tr '[:upper:]' '[:lower:]')
exec uvicorn main:app --host 0.0.0.0 --port 8000 --reload --log-level $UVICORN_LOG_LEVEL --timeout-keep-alive 1800 --timeout-graceful-shutdown 1800 