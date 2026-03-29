import os
import json
from typing import Dict, Optional, Any
import openai
from dotenv import load_dotenv
import logging

logger = logging.getLogger(__name__)

# Enhanced metadata schema supporting papers, books, and web sources
METADATA_SCHEMA = {
    "type": "object",
    "properties": {
        "document_type": {
            "type": "string",
            "enum": ["paper", "book", "legal", "institutional", "web", "other"],
            "description": "Type of document: 'paper' (academic paper/journal article/working paper/thesis), 'book' (textbook/monograph/edited volume/handbook/lexicon), 'legal' (law text/statute/regulation/court ruling), 'institutional' (government report/central bank publication/policy document), 'web' (web article/blog post/news), or 'other'"
        },
        "title": {"type": "string", "description": "The full title of the document"},
        "authors": {
            "type": "array",
            "items": {"type": "string"},
            "description": "List of author names. Use empty array if no authors found."
        },
        "publication_year": {"type": ["integer", "null"], "description": "Publication year"},
        "keywords": {
            "type": "array",
            "items": {"type": "string"},
            "description": "List of relevant keywords or topics"
        },
        "description": {"type": ["string", "null"], "description": "Abstract, summary, or description of the content"},

        # Paper-specific fields
        "journal_or_source": {"type": ["string", "null"], "description": "For papers: journal, conference, or preprint server (e.g., 'Nature', 'arXiv')"},
        "doi": {"type": ["string", "null"], "description": "For papers: Digital Object Identifier"},

        # Book-specific fields
        "publisher": {"type": ["string", "null"], "description": "For books: publisher name"},
        "edition": {"type": ["string", "null"], "description": "For books: edition information (e.g., '2nd Edition', 'Revised')"},
        "isbn": {"type": ["string", "null"], "description": "For books: ISBN number"},
        "page_count": {"type": ["integer", "null"], "description": "For books: total number of pages"},
        "chapters": {
            "type": "array",
            "items": {"type": "string"},
            "description": "For books: list of main chapter titles from table of contents (limit to first 10-15)"
        },

        # Web source-specific fields
        "url": {"type": ["string", "null"], "description": "For web sources: original URL if mentioned in the text"},
        "website_name": {"type": ["string", "null"], "description": "For web sources: name of the website or blog"},
        "organization": {"type": ["string", "null"], "description": "For web sources or reports: organization/company name"}
    },
    "required": ["document_type", "title"] # Minimal requirements
}

# Example metadata for different document types
METADATA_EXAMPLES = {
    "paper": {
        "document_type": "paper",
        "title": "Attention Is All You Need",
        "authors": ["Ashish Vaswani", "Noam Shazeer", "Niki Parmar"],
        "journal_or_source": "arXiv",
        "publication_year": 2017,
        "doi": "10.48550/arXiv.1706.03762",
        "description": "The dominant sequence transduction models are based on complex recurrent or convolutional neural networks...",
        "keywords": ["attention mechanism", "transformer", "NLP"]
    },
    "book": {
        "document_type": "book",
        "title": "Deep Learning",
        "authors": ["Ian Goodfellow", "Yoshua Bengio", "Aaron Courville"],
        "publisher": "MIT Press",
        "publication_year": 2016,
        "isbn": "978-0262035613",
        "edition": "1st Edition",
        "page_count": 800,
        "description": "An introduction to a broad range of topics in deep learning...",
        "chapters": ["Introduction", "Linear Algebra", "Probability", "Neural Networks"],
        "keywords": ["deep learning", "machine learning", "neural networks"]
    },
    "web": {
        "document_type": "web",
        "title": "The State of AI in 2024",
        "authors": ["Jane Smith"],
        "website_name": "TechCrunch",
        "organization": "TechCrunch",
        "publication_year": 2024,
        "description": "An analysis of current trends in artificial intelligence...",
        "keywords": ["AI", "technology", "trends"]
    }
}

class MetadataExtractor:
    """
    Extracts structured metadata from document text using an LLM.
    """
    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: str = "https://openrouter.ai/api/v1/",
        model: str = "openai/gpt-4o-mini", # Or another suitable model
        max_text_sample: int = 4000 # Characters to send to LLM
    ):
        load_dotenv() # Load .env file if present
        self.api_key = api_key or os.getenv("OPENROUTER_API_KEY")
        self.base_url = base_url
        self.model = model
        self.max_text_sample = max_text_sample

        if not self.api_key:
            logger.debug("API key not provided during initialization. Will be configured from user settings when available.")
            self.client = None
        else:
            try:
                self.client = openai.OpenAI(
                    base_url=self.base_url,
                    api_key=self.api_key
                )
            except Exception as e:
                logger.error(f"Error initializing OpenAI client: {e}")
                self.client = None

    @classmethod
    def from_user_settings(cls, user_settings: Dict[str, Any], max_text_sample: int = 4000) -> 'MetadataExtractor':
        """
        Create a MetadataExtractor instance from user settings.
        
        Args:
            user_settings: User settings dictionary containing AI provider configuration
            max_text_sample: Maximum characters to send to LLM
            
        Returns:
            MetadataExtractor instance configured with user's AI provider settings
        """
        # Get AI endpoints settings from user settings
        ai_endpoints = user_settings.get('ai_endpoints', {})
        
        # Use the fast model configuration for metadata extraction (fast and efficient)
        fast_model_config = ai_endpoints.get('advanced_models', {}).get('fast', {})
        
        # Extract configuration from the fast model
        api_key = fast_model_config.get('api_key')
        base_url = fast_model_config.get('base_url')
        model = fast_model_config.get('model_name', 'openai/gpt-4o-mini')
        
        # If base_url is None/empty, get it from the provider this model uses
        if not base_url:
            provider_name = fast_model_config.get('provider', 'openrouter')
            providers = ai_endpoints.get('providers', {})
            provider_config = providers.get(provider_name, {})
            base_url = provider_config.get('base_url', 'https://openrouter.ai/api/v1/')
        
        # If no fast model configuration found, fallback to environment variables
        if not api_key or not model:
            logger.warning("No fast model configuration found in user settings, falling back to environment variables")
            api_key = None  # Will be loaded from environment in __init__
            base_url = "https://openrouter.ai/api/v1/"
            model = "openai/gpt-4o-mini"
        else:
            logger.info(f"Using user's fast model for metadata extraction: {model} at {base_url}")
        
        return cls(
            api_key=api_key,
            base_url=base_url,
            model=model,
            max_text_sample=max_text_sample
        )

    def extract(self, text_sample: str) -> Optional[Dict[str, Any]]:
        """
        Extracts metadata from the provided text sample using the configured LLM.

        Args:
            text_sample: The text snippet (ideally from the start of the document)
                         to extract metadata from.

        Returns:
            A dictionary containing the extracted metadata, or None if extraction fails.
        """
        if not self.client:
            print("MetadataExtractor: LLM client not initialized. Cannot extract metadata.")
            return None
        if not text_sample:
            print("MetadataExtractor: No text sample provided.")
            return None

        # Limit the text sample size – include beginning AND end of document
        # The end often contains colophon, imprint page with publisher/ISBN/year
        head_size = self.max_text_sample
        tail_size = 2000
        if len(text_sample) > head_size + tail_size:
            text_sample_truncated = (
                text_sample[:head_size]
                + "\n\n[...]\n\n"
                + text_sample[-tail_size:]
            )
        else:
            text_sample_truncated = text_sample[:head_size + tail_size]
        
        # Debug: Print the first part of the text sample
        print(f"MetadataExtractor: Processing text sample (first 500 chars):")
        print(f"{text_sample_truncated[:500]}...")
        print(f"MetadataExtractor: Total text sample length: {len(text_sample_truncated)} chars")

        # Construct the prompt
        system_prompt = "You are a meticulous metadata extraction assistant. You always return valid JSON conforming exactly to the provided schema. Extract information based *only* on the provided text."
        user_prompt = f"""Extract metadata from the following document text snippet. Follow the JSON schema precisely.

IMPORTANT INSTRUCTIONS:
1. First determine the document_type: 'paper' (academic paper), 'book', 'web' (web article/blog), or 'other'
2. Extract ALL relevant fields based on the document type
3. Use `null` for fields you cannot confidently determine
4. For arrays (authors, keywords, chapters), use empty array [] if none found
5. Extract chapter titles from table of contents for books (limit to first 10-15 chapters)

JSON Schema:
```json
{json.dumps(METADATA_SCHEMA, indent=2)}
```

Example Outputs by Document Type:

For Academic Papers:
```json
{json.dumps(METADATA_EXAMPLES["paper"], indent=2)}
```

For Books:
```json
{json.dumps(METADATA_EXAMPLES["book"], indent=2)}
```

For Web Sources:
```json
{json.dumps(METADATA_EXAMPLES["web"], indent=2)}
```

Document Text Snippet:
---
{text_sample_truncated}
---

Extract the metadata based *only* on the text provided above and return it as JSON matching the schema. Include only fields relevant to the document type you detected.
"""

        messages = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": user_prompt}
        ]

        print(f"MetadataExtractor: Sending request to {self.model}...")
        
        # Check if this is a GPT-5 model that requires special handling
        is_gpt5_model = any(x in self.model.lower() for x in ['gpt-5', 'gpt5'])
        is_openai_api = 'api.openai.com' in self.base_url or 'openai.azure.com' in self.base_url
        
        try:
            if is_gpt5_model and is_openai_api:
                print(f"MetadataExtractor: Using GPT-5 specific parameters for {self.model}")
                print(f"MetadataExtractor: Base URL: {self.base_url}")
                print(f"MetadataExtractor: API key present: {bool(self.api_key)}")
                # GPT-5 models via OpenAI API require special parameters
                response = self.client.chat.completions.create(
                    model=self.model,
                    messages=messages,
                    max_completion_tokens=1000,  # Use max_completion_tokens for GPT-5
                    # Don't set temperature for GPT-5 (use default)
                    response_format={
                        "type": "json_object"
                    }
                )
            else:
                # Standard parameters for non-GPT-5 models
                response = self.client.chat.completions.create(
                    model=self.model,
                    messages=messages,
                    max_tokens=1000, # Adjust as needed
                    temperature=0.1, # Low temperature for factual extraction
                    response_format={
                        "type": "json_object", # Use json_object type for general JSON
                        # Note: OpenAI API might support json_schema directly,
                        # but json_object is more broadly compatible with OpenRouter models
                        # If using OpenAI directly, you might use:
                        # "type": "json_schema",
                        # "json_schema": {
                        #     "name": "document_metadata",
                        #     "schema": METADATA_SCHEMA
                        # }
                    }
                )

            response_content = response.choices[0].message.content
            print("MetadataExtractor: Received response from LLM.")
            print(f"MetadataExtractor: Response content length: {len(response_content) if response_content else 0}")
            if response_content and len(response_content) < 1000:
                print(f"MetadataExtractor: Raw response: {response_content[:500]}")  # Show first 500 chars for debugging

            if not response_content:
                 print("MetadataExtractor: LLM returned empty content.")
                 return None

            # Parse the JSON response
            metadata = json.loads(response_content)
            print("MetadataExtractor: Successfully parsed JSON response.")

            # Basic validation
            if not isinstance(metadata, dict):
                 print(f"MetadataExtractor: LLM response is not a JSON object: {type(metadata)}")
                 return None
            if "title" not in metadata or not metadata["title"]:
                 print("MetadataExtractor: Error - Extracted metadata missing required 'title'.")
                 return None

            # Ensure document_type is set
            if "document_type" not in metadata:
                print("MetadataExtractor: Warning - 'document_type' missing, defaulting to 'other'.")
                metadata["document_type"] = "other"

            # Ensure authors field exists
            if "authors" not in metadata or not isinstance(metadata["authors"], list):
                print("MetadataExtractor: Warning - 'authors' field is missing or invalid, using empty list.")
                metadata["authors"] = []

            # Debug: Print the extracted metadata based on document type
            doc_type = metadata.get('document_type', 'other')
            print(f"MetadataExtractor: Extracted metadata for {doc_type.upper()}:")
            print(f"  - Title: {metadata.get('title', 'N/A')}")
            print(f"  - Authors: {metadata.get('authors', [])}")
            print(f"  - Year: {metadata.get('publication_year', 'N/A')}")
            print(f"  - Keywords: {metadata.get('keywords', [])}")

            if doc_type == "paper":
                print(f"  - Journal: {metadata.get('journal_or_source', 'N/A')}")
                print(f"  - DOI: {metadata.get('doi', 'N/A')}")
                if metadata.get('description'):
                    print(f"  - Abstract: {metadata.get('description', '')[:100]}...")
            elif doc_type == "book":
                print(f"  - Publisher: {metadata.get('publisher', 'N/A')}")
                print(f"  - Edition: {metadata.get('edition', 'N/A')}")
                print(f"  - ISBN: {metadata.get('isbn', 'N/A')}")
                print(f"  - Pages: {metadata.get('page_count', 'N/A')}")
                chapters = metadata.get('chapters', [])
                if chapters:
                    print(f"  - Chapters: {len(chapters)} chapters extracted")
            elif doc_type == "web":
                print(f"  - Website: {metadata.get('website_name', 'N/A')}")
                print(f"  - Organization: {metadata.get('organization', 'N/A')}")
                print(f"  - URL: {metadata.get('url', 'N/A')}")

            if metadata.get('description'):
                print(f"  - Description: {metadata.get('description', '')[:100]}...")

            # Optional: Check for publication_year if you want to be even stricter,
            # but it's not required by the current schema definition.
            # if "publication_year" not in metadata or metadata["publication_year"] is None:
            #     print("MetadataExtractor: Warning - Extracted metadata missing 'publication_year'.")

            return metadata

        except json.JSONDecodeError as e:
            print(f"MetadataExtractor: Error decoding JSON response from LLM: {e}")
            print(f"Raw response content was:\n{response_content}")
            return None
        except openai.APIError as e:
            print(f"MetadataExtractor: OpenAI API error: {e}")
            
            # Check for GPT-5 specific errors and retry with correct parameters
            error_str = str(e)
            if is_gpt5_model and is_openai_api and (
                "max_tokens" in error_str.lower() or 
                "maximum" in error_str.lower() or
                "temperature" in error_str.lower()
            ):
                print(f"MetadataExtractor: Detected GPT-5 parameter error, retrying with correct parameters...")
                try:
                    # Retry with GPT-5 specific parameters
                    response = self.client.chat.completions.create(
                        model=self.model,
                        messages=messages,
                        max_completion_tokens=1000,  # Use max_completion_tokens for GPT-5
                        # Don't set temperature for GPT-5
                        response_format={
                            "type": "json_object"
                        }
                    )
                    response_content = response.choices[0].message.content
                    print("MetadataExtractor: GPT-5 retry successful")
                    
                    if not response_content:
                        print("MetadataExtractor: LLM returned empty content on retry.")
                        return None
                    
                    metadata = json.loads(response_content)
                    print("MetadataExtractor: Successfully parsed JSON response from GPT-5 retry.")

                    # Validate the metadata
                    if not isinstance(metadata, dict):
                        print(f"MetadataExtractor: LLM response is not a JSON object: {type(metadata)}")
                        return None
                    if "title" not in metadata or not metadata["title"]:
                        print("MetadataExtractor: Error - Extracted metadata missing required 'title'.")
                        return None

                    # Ensure document_type is set
                    if "document_type" not in metadata:
                        print("MetadataExtractor: Warning - 'document_type' missing in retry, defaulting to 'other'.")
                        metadata["document_type"] = "other"

                    # Ensure authors field exists
                    if "authors" not in metadata or not isinstance(metadata["authors"], list):
                        print("MetadataExtractor: Warning - 'authors' field is missing or invalid in retry, using empty list.")
                        metadata["authors"] = []

                    return metadata
                    
                except Exception as retry_e:
                    print(f"MetadataExtractor: GPT-5 retry failed: {retry_e}")
                    return None
            return None
        except Exception as e:
            print(f"MetadataExtractor: An unexpected error occurred: {e}")
            return None

    def extract_and_enrich_sync(self, text_sample: str, filename: str = "") -> Optional[Dict[str, Any]]:
        """Extract metadata via LLM, then enrich with external databases (synchronous wrapper).

        This is meant to be called from synchronous code (e.g. the background
        document processor).  It runs the async enrichment pipeline in a new
        event loop if necessary.
        """
        # Step 1: LLM extraction (synchronous)
        metadata = self.extract(text_sample)

        # Step 2: Async enrichment
        try:
            from services.metadata_enrichment import enrich_metadata
            import asyncio

            async def _do_enrich():
                # Reset the shared httpx client to avoid "Event loop is closed" errors
                # when called from a fresh event loop in a thread
                from services import metadata_enrichment
                metadata_enrichment._http_client = None
                return await enrich_metadata(
                    existing_metadata=metadata or {},
                    document_text=text_sample,
                    filename=filename,
                )

            # Always run in a new thread with a fresh event loop to avoid
            # "Event loop is closed" errors on subsequent calls
            import concurrent.futures
            with concurrent.futures.ThreadPoolExecutor() as pool:
                enriched = pool.submit(lambda: asyncio.run(_do_enrich())).result(timeout=60)

            if enriched:
                print(f"MetadataExtractor: Enrichment complete – "
                      f"completeness={enriched.get('metadata_completeness', 'N/A')}, "
                      f"sources={enriched.get('metadata_sources', [])}")
                return enriched

        except Exception as e:
            print(f"MetadataExtractor: Enrichment failed (non-fatal): {e}")
            # Fall through and return LLM-only metadata

        return metadata
