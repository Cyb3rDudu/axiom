from sqlalchemy import create_engine, pool, text
from sqlalchemy.engine import Engine
from sqlalchemy.exc import OperationalError
from sqlalchemy.ext.declarative import declarative_base
from sqlalchemy.orm import sessionmaker
import os
import time
import logging

logger = logging.getLogger(__name__)

# PostgreSQL connection URL from environment
# Format: postgresql://user:password@host:port/database
DATABASE_URL = os.getenv(
    "DATABASE_URL", 
    "postgresql://axiom_user:axiom_password@postgres:5432/axiom_db"
)


# ---------------------------------------------------------------------------
# Hardened engine configuration
# ---------------------------------------------------------------------------
# These machines reach Postgres through a macvlan bridge (container netns
# 192.168.1.107 -> host-published 192.168.1.2:5432). Right after a podman
# rebuild/restart that path can briefly drop a connection mid-handshake, which
# previously (a) hung the backend's init_postgres migration runner on a blocked
# socket read, and (b) invalidated the doc-processor's SQLAlchemy pool so it
# spun forever without recovering. The settings below make connections
# self-healing instead.
#
#   pool_pre_ping  -> discard a dead/stale connection before handing it out
#   pool_recycle   -> proactively rebuild connections before the macvlan path
#                     can consider them idle/dead
#   connect_timeout-> fail a stuck TCP handshake fast instead of hanging
#   TCP keepalives -> detect a half-open connection (the case connect_timeout
#                     alone misses, since the handshake already succeeded) and
#                     surface it as an error the pool can recover from
#   statement_timeout -> never let a single query block indefinitely


def make_engine(url: str = DATABASE_URL, *, app_name: str = "axiom") -> Engine:
    """Create a resilience-hardened SQLAlchemy engine.

    All engine creation in the codebase should go through here so the pool,
    timeout and keepalive settings are consistent. SQLite (dev) is handled
    with minimal args.
    """
    if url.startswith("sqlite"):
        return create_engine(
            url,
            connect_args={"check_same_thread": False},
            echo=False,
        )

    return create_engine(
        url,
        pool_size=50,
        max_overflow=30,
        pool_pre_ping=True,     # discard dead connections before use
        pool_recycle=900,       # rebuild every 15 min (shorter than macvlan idle paths)
        pool_timeout=30,        # wait up to 30s for a free connection
        echo=False,
        future=True,
        connect_args={
            "connect_timeout": 10,
            # TCP keepalives: probe after 30s idle, every 10s, drop after 3
            # failed probes. This is what catches a half-open macvlan socket
            # that connect_timeout cannot (handshake already completed).
            "keepalives": 1,
            "keepalives_idle": 30,
            "keepalives_interval": 10,
            "keepalives_count": 3,
            "application_name": app_name,
            "options": "-c statement_timeout=30000",  # 30s per statement
        },
    )


def connect_with_retries(
    engine_obj: Engine,
    *,
    max_retries: int = 15,
    base_delay: float = 2.0,
    purpose: str = "connect",
) -> bool:
    """Open a working connection to the DB, retrying through transient drops.

    On each attempt we try a trivial ``SELECT 1``. If that raises
    ``OperationalError`` (the macvlan timeout / refused / pool-invalidation
    class), we ``engine.dispose()`` to reset any invalidated pool state and
    back off before retrying. This is the single recovery primitive that turns
    a restart-time macvlan blip from "process hangs / spins forever" into
    "self-heals within ~30-60s".

    Returns True once a connection succeeded, False if all retries were spent.
    """
    last_err = None
    for attempt in range(1, max_retries + 1):
        try:
            with engine_obj.connect() as conn:
                conn.execute(text("SELECT 1")).close()
            if attempt > 1:
                logger.info(
                    "DB %s recovered on attempt %d/%d", purpose, attempt, max_retries
                )
            return True
        except OperationalError as e:
            last_err = e
            logger.warning(
                "DB %s failed (attempt %d/%d): %s; disposing pool and retrying in %.1fs",
                purpose, attempt, max_retries, _short_err(e), base_delay,
            )
            # Reset the invalidated pool so the next attempt gets a fresh
            # connect instead of re-raising the cached pool-invalidation error.
            try:
                engine_obj.dispose()
            except Exception as disp_err:  # never let dispose mask the real error
                logger.warning("engine.dispose() during retry failed: %s", disp_err)
            time.sleep(base_delay)
            # Gentle backoff capped at ~10s so we don't stall too long.
            base_delay = min(base_delay * 1.5, 10.0)
        except Exception as e:
            last_err = e
            logger.warning(
                "DB %s hit unexpected error (attempt %d/%d): %s",
                purpose, attempt, max_retries, e,
            )
            time.sleep(base_delay)
    logger.error(
        "DB %s did not recover after %d attempts: %s", purpose, max_retries, last_err
    )
    return False


def _short_err(e: Exception) -> str:
    """One-line summary of an OperationalError for logs."""
    msg = str(getattr(e, "orig", e) or e)
    return " ".join(msg.split())[:160]


# Check if we're using SQLite (for backward compatibility in development)
if DATABASE_URL.startswith("sqlite"):
    engine = make_engine(DATABASE_URL)
    logger.warning("Using SQLite database - consider migrating to PostgreSQL for production")
else:
    engine = make_engine(DATABASE_URL, app_name="axiom")
    logger.info(f"Connected to PostgreSQL database: {DATABASE_URL.split('@')[1].split('/')[0]}")

SessionLocal = sessionmaker(autocommit=False, autoflush=False, bind=engine)

Base = declarative_base()

def get_db():
    """Dependency to get database session"""
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()

def init_db():
    """Initialize database tables"""
    try:
        # Import all models to ensure they're registered with Base
        from . import models
        
        # Create all tables
        Base.metadata.create_all(bind=engine)
        logger.info("Database tables initialized successfully")
    except Exception as e:
        logger.error(f"Failed to initialize database: {str(e)}")
        raise

def test_connection() -> bool:
    """Test database connection (single attempt, no retry).

    For startup-time resilience use :func:`connect_with_retries` instead —
    this function is kept lightweight for ad-hoc probes.
    """
    try:
        with engine.connect() as conn:
            result = conn.execute(text("SELECT 1"))
            result.fetchone()
        logger.info("Database connection test successful")
        return True
    except Exception as e:
        logger.error(f"Database connection test failed: {str(e)}")
        return False
