#!/usr/bin/env python3
"""
PostgreSQL Database Initialization Script
Ensures the database is properly set up with all required extensions and tables
"""

import os
import sys
import time
import logging
from sqlalchemy import create_engine, text
from sqlalchemy.exc import OperationalError

# Add parent directory to path
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from database.database import (
    DATABASE_URL,
    Base,
    engine,
    init_db,
    test_connection,
    connect_with_retries,
)
from database import models  # Import all models

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

def wait_for_database(max_retries: int = 15, retry_interval: float = 2.0):
    """Wait for the database to become reachable, self-healing through the
    transient macvlan drop that can occur right after a podman restart.

    Uses :func:`connect_with_retries`, which disposes the pool on each failed
    attempt so we never get stuck on an invalidated pool (the previous failure
    mode: the migration runner hung at 0% CPU on a blocked socket read).
    """
    if connect_with_retries(
        engine, max_retries=max_retries, base_delay=retry_interval, purpose="startup"
    ):
        logger.info("Database is ready!")
        return True
    logger.error("Database did not become available in time")
    return False

def ensure_extensions():
    """Ensure required PostgreSQL extensions are installed"""
    try:
        with engine.connect() as conn:
            # Ensure UUID extension
            result = conn.execute(text(
                "SELECT * FROM pg_extension WHERE extname = 'uuid-ossp'"
            ))
            if not result.fetchone():
                conn.execute(text("CREATE EXTENSION IF NOT EXISTS \"uuid-ossp\";"))
                conn.commit()
                logger.info("UUID extension installed")
            else:
                logger.info("UUID extension already exists")
            
            # Ensure pgvector extension
            result = conn.execute(text(
                "SELECT * FROM pg_extension WHERE extname = 'vector'"
            ))
            if not result.fetchone():
                conn.execute(text("CREATE EXTENSION IF NOT EXISTS vector;"))
                conn.commit()
                logger.info("PGVector extension installed")
            else:
                logger.info("PGVector extension already exists")
    except Exception as e:
        logger.error(f"Failed to ensure extensions: {str(e)}")
        raise

def create_tables():
    """Create all database tables"""
    try:
        # Create all tables defined in models
        Base.metadata.create_all(bind=engine)
        logger.info("All database tables created successfully")
        
        # List created tables
        with engine.connect() as conn:
            result = conn.execute(text(
                "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"
            ))
            tables = [row[0] for row in result]
            logger.info(f"Created tables: {', '.join(tables)}")
            
    except Exception as e:
        logger.error(f"Failed to create tables: {str(e)}")
        raise

def create_default_admin():
    """Create a default admin user if none exists"""
    from sqlalchemy.orm import Session
    from passlib.context import CryptContext
    from datetime import datetime, timezone
    
    pwd_context = CryptContext(schemes=["bcrypt"], deprecated="auto")
    
    try:
        with Session(engine) as session:
            # Check if any admin exists
            admin_exists = session.query(models.User).filter_by(is_admin=True).first()
            if not admin_exists:
                # Get admin credentials from environment or use defaults
                admin_username = os.environ.get('ADMIN_USERNAME', 'admin')
                admin_password = os.environ.get('ADMIN_PASSWORD', 'admin123')
                
                # Create default admin
                admin = models.User(
                    username=admin_username,
                    email="admin@axiom.local",  # Added email field
                    hashed_password=pwd_context.hash(admin_password),
                    full_name="System Administrator",
                    is_admin=True,
                    is_active=True,
                    role="admin",
                    user_type="admin",
                    created_at=datetime.now(timezone.utc),
                    updated_at=datetime.now(timezone.utc)
                )
                session.add(admin)
                session.commit()
                logger.info(f"Default admin user created (username: {admin_username})")
                if admin_password == 'admin123':
                    logger.warning("⚠️  IMPORTANT: Using default password - change immediately!")
                else:
                    logger.info("Admin user created with custom password from environment")
            else:
                logger.info("Admin user already exists")
    except Exception as e:
        logger.error(f"Failed to create default admin: {str(e)}")

def verify_database_setup():
    """Verify that the database is properly set up"""
    try:
        with engine.connect() as conn:
            # Check UUID extension
            result = conn.execute(text(
                "SELECT * FROM pg_extension WHERE extname = 'uuid-ossp'"
            ))
            if not result.fetchone():
                logger.error("UUID extension not found!")
                return False
            
            # Check pgvector extension
            result = conn.execute(text(
                "SELECT * FROM pg_extension WHERE extname = 'vector'"
            ))
            if not result.fetchone():
                logger.error("PGVector extension not found!")
                return False
            
            # Check critical tables
            critical_tables = [
                'users', 'documents', 'document_groups', 
                'chats', 'messages', 'writing_sessions'
            ]
            
            for table in critical_tables:
                result = conn.execute(text(
                    f"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = '{table}')"
                ))
                if not result.scalar():
                    logger.error(f"Table '{table}' not found!")
                    return False
            
            logger.info("Database setup verified successfully")
            return True
            
    except Exception as e:
        logger.error(f"Database verification failed: {str(e)}")
        return False

def run_column_migrations():
    """Add new columns to existing tables if they don't exist."""
    migrations = [
        ("users", "api_key", "VARCHAR UNIQUE"),
    ]
    try:
        with engine.connect() as conn:
            for table, column, col_type in migrations:
                result = conn.execute(text(
                    "SELECT 1 FROM information_schema.columns "
                    "WHERE table_name = :table AND column_name = :column"
                ), {"table": table, "column": column})
                if not result.fetchone():
                    conn.execute(text(f'ALTER TABLE "{table}" ADD COLUMN "{column}" {col_type}'))
                    conn.commit()
                    logger.info(f"Added column {table}.{column}")
                else:
                    logger.debug(f"Column {table}.{column} already exists")
    except Exception as e:
        logger.error(f"Column migration failed: {e}")


def run_sql_migrations():
    """Run SQL migration files for existing databases.

    Each migration executes inside a retry loop: a transient macvlan drop mid-
    run previously hung the backend here (migration 05) on a blocked socket
    read with no timeout. On ``OperationalError`` we dispose the pool and retry
    the whole migration so a restart-time blip is self-healing instead of fatal.

    Returns the list of migration filenames that genuinely FAILED (i.e. could
    not be applied or confirmed already-applied after all retries). The caller
    (``main``) fails fast on a non-empty list rather than booting the app with
    missing schema (review finding 1).
    """
    failed: list[str] = []
    try:
        import glob
        import os

        from sqlalchemy.exc import OperationalError

        # Get the init-db directory path
        init_db_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), 'init-db')

        if os.path.exists(init_db_dir):
            # Get all SQL files sorted by name
            sql_files = sorted(glob.glob(os.path.join(init_db_dir, '*.sql')))

            for sql_file in sql_files:
                filename = os.path.basename(sql_file)
                # Skip the main schema files, only run migration files
                # Run migration files that start with 03- or higher numbers
                if any(filename.startswith(f'{num:02d}-') for num in range(3, 100)):
                    logger.info(f"Running migration: {filename}")
                    with open(sql_file, 'r') as f:
                        sql_content = f.read()

                    outcome = "pending"  # "applied" | "already_applied" | "failed"
                    last_db_err = None
                    for attempt in range(1, 4):  # up to 3 attempts per migration
                        try:
                            with engine.connect() as conn:
                                conn.execute(text(sql_content))
                                conn.commit()
                            outcome = "applied"
                            break
                        except OperationalError as oe:
                            last_db_err = oe
                            logger.warning(
                                "Migration %s DB error (attempt %d/3): %s — disposing pool, retrying",
                                filename, attempt, str(getattr(oe, "orig", oe))[:160],
                            )
                            try:
                                engine.dispose()
                            except Exception:
                                pass
                            time.sleep(3)
                    if outcome == "applied":
                        logger.info(f"✅ Migration {filename} completed")
                    elif outcome == "pending":
                        # OperationalError on every retry, or a non-DB exception.
                        # Distinguish: a non-DB ProgrammingError here usually means
                        # the migration is already applied (e.g. "column already
                        # exists") — treat that as already-applied, not a failure.
                        # A persistent OperationalError is a real failure.
                        if last_db_err is not None:
                            logger.error(
                                "Migration %s FAILED after retries: %s",
                                filename, str(getattr(last_db_err, "orig", last_db_err))[:200],
                            )
                            failed.append(filename)
                        else:
                            logger.warning(
                                "Migration %s treated as already applied (non-DB error)",
                                filename,
                            )

        logger.info("SQL migrations check completed")

    except Exception as e:
        logger.error(f"Error running SQL migrations: {str(e)}")
        failed.append("<migrations-runner>")

    return failed

def main():
    """Main initialization function"""
    logger.info("Starting PostgreSQL database initialization...")
    
    # Skip if using SQLite
    if DATABASE_URL.startswith("sqlite"):
        logger.info("Using SQLite database, skipping PostgreSQL initialization")
        init_db()
        return
    
    # Wait for database to be available
    if not wait_for_database():
        sys.exit(1)
    
    # Ensure required extensions
    ensure_extensions()
    
    # Create tables
    create_tables()

    # Run column migrations for new columns on existing tables
    run_column_migrations()

    # Run SQL migrations for existing databases; fail fast if any genuinely
    # failed so we never boot the app with missing migration schema. The init
    # container / systemd unit will restart and retry (review finding 1).
    migration_failures = run_sql_migrations()
    if migration_failures:
        logger.error(
            "❌ %d SQL migration(s) failed: %s — aborting startup",
            len(migration_failures), migration_failures,
        )
        sys.exit(1)

    # Create default admin
    create_default_admin()
    
    # Verify setup
    if verify_database_setup():
        logger.info("✅ PostgreSQL database initialization completed successfully!")
    else:
        logger.error("❌ Database setup verification failed")
        sys.exit(1)

if __name__ == "__main__":
    main()