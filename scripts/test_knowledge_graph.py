#!/usr/bin/env python3
"""
Knowledge Graph Testing Script

Tests the knowledge graph implementation to ensure all components work correctly.
"""

import sys
import os

# Add backend to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'axiom_backend'))

import asyncio
from sqlalchemy import text
from database.database import get_db
from ai_researcher import config


def test_config():
    """Test configuration is loaded correctly."""
    print("\n=== Testing Configuration ===")
    print(f"ENABLE_KNOWLEDGE_GRAPH: {config.ENABLE_KNOWLEDGE_GRAPH}")
    print(f"ENABLE_GRAPH_RETRIEVAL: {config.ENABLE_GRAPH_RETRIEVAL}")
    print(f"GRAPH_RETRIEVAL_CONFIG: {config.GRAPH_RETRIEVAL_CONFIG}")
    print(f"ENTITY_EXTRACTION_CONFIG: {config.ENTITY_EXTRACTION_CONFIG}")
    print("✓ Configuration loaded")


def test_database_tables():
    """Test database tables exist."""
    print("\n=== Testing Database Tables ===")

    db = next(get_db())
    try:
        tables = [
            'document_entities',
            'entity_chunk_occurrences',
            'entity_relationships',
            'relationship_evidence',
            'chunk_relationships'
        ]

        for table in tables:
            query = text(f"SELECT COUNT(*) FROM {table}")
            count = db.execute(query).scalar()
            print(f"✓ Table '{table}' exists ({count} rows)")

    except Exception as e:
        print(f"✗ Database test failed: {e}")
        return False
    finally:
        db.close()

    return True


def test_graph_store():
    """Test GraphStore functionality."""
    print("\n=== Testing Graph Store ===")

    try:
        from ai_researcher.core_rag.graph_store import GraphStore

        store = GraphStore()
        print("✓ GraphStore imported and initialized")

        # Test entity creation
        entity_id = store.add_entity(
            entity_text="Test Entity",
            entity_type="CONCEPT",
            canonical_form="test entity"
        )
        print(f"✓ Entity created: {entity_id}")

        return True
    except Exception as e:
        print(f"✗ GraphStore test failed: {e}")
        import traceback
        traceback.print_exc()
        return False


def test_entity_extractor():
    """Test EntityExtractor functionality."""
    print("\n=== Testing Entity Extractor ===")

    try:
        from ai_researcher.core_rag.entity_extractor import EntityExtractor

        extractor = EntityExtractor()
        print("✓ EntityExtractor imported and initialized")

        if extractor.nlp:
            print("✓ spaCy model loaded")
        else:
            print("⚠ spaCy model not available - run: python -m spacy download en_core_web_sm")

        return True
    except Exception as e:
        print(f"✗ EntityExtractor test failed: {e}")
        import traceback
        traceback.print_exc()
        return False


async def test_retriever():
    """Test Retriever with graph enhancement."""
    print("\n=== Testing Retriever ===")

    try:
        from ai_researcher.core_rag.embedder import TextEmbedder
        from ai_researcher.core_rag.pgvector_store import PGVectorStore
        from ai_researcher.core_rag.retriever import Retriever

        embedder = TextEmbedder()
        vector_store = PGVectorStore()
        retriever = Retriever(embedder, vector_store)

        print("✓ Retriever initialized")

        if retriever.graph_retriever:
            print("✓ Graph-enhanced retrieval enabled")
        else:
            print("⚠ Graph-enhanced retrieval disabled (set ENABLE_GRAPH_RETRIEVAL=true to enable)")

        return True
    except Exception as e:
        print(f"✗ Retriever test failed: {e}")
        import traceback
        traceback.print_exc()
        return False


def test_api_imports():
    """Test API endpoints can be imported."""
    print("\n=== Testing API Imports ===")

    try:
        from api import rag
        print("✓ RAG API module imported")
        print(f"✓ Found {len(rag.router.routes)} routes")

        for route in rag.router.routes:
            if hasattr(route, 'path'):
                print(f"  - {route.methods} {route.path}")

        return True
    except Exception as e:
        print(f"✗ API import test failed: {e}")
        import traceback
        traceback.print_exc()
        return False


def main():
    """Run all tests."""
    print("=================================")
    print("Knowledge Graph Testing Suite")
    print("=================================")

    tests = [
        ("Configuration", test_config),
        ("Database Tables", test_database_tables),
        ("Graph Store", test_graph_store),
        ("Entity Extractor", test_entity_extractor),
        ("Retriever", lambda: asyncio.run(test_retriever())),
        ("API Imports", test_api_imports),
    ]

    results = {}
    for name, test_func in tests:
        try:
            result = test_func()
            results[name] = result if result is not None else True
        except Exception as e:
            print(f"\n✗ {name} test crashed: {e}")
            results[name] = False

    # Summary
    print("\n=================================")
    print("Test Summary")
    print("=================================")

    passed = sum(1 for r in results.values() if r)
    total = len(results)

    for name, result in results.items():
        status = "✓ PASS" if result else "✗ FAIL"
        print(f"{status}: {name}")

    print(f"\nTotal: {passed}/{total} tests passed")

    if passed == total:
        print("\n✓ All tests passed! Knowledge graph is ready to use.")
        return 0
    else:
        print(f"\n✗ {total - passed} test(s) failed. Check output above for details.")
        return 1


if __name__ == "__main__":
    sys.exit(main())
