# Roadmap

## Current State (March 2026)

AXIOM has been actively developed since forking from Maestro, with 170+ commits adding major features and improvements. The project is in active alpha development.

### Recently Delivered

These features have been implemented since the fork:

- **Native AI Providers** - DeepSeek and Z.AI (Zhipu GLM) as first-class providers with special handling
- **Citation System** - Configurable citation profiles (Numbered, KMU Akademie APA 7, APA 7 English, custom) with per-mission overrides and author-year bibliography generation
- **RAG Enhancements** - Knowledge graph entity extraction, OpenSearch BM25 fulltext search, hybrid retrieval with Reciprocal Rank Fusion, structure-aware chunking
- **Document-Aware Chat** - RAG-grounded chat responses with embedded document images
- **Apple Silicon Support** - MPS GPU acceleration for native macOS deployment, device-agnostic GPU memory management
- **Provider Robustness** - 3-level JSON response format fallback (json_schema → json_object → prompt-only), context window truncation with correction factor retry
- **Multilingual Support** - Language code propagation through all agents and prompts
- **Cost Tracking** - Generalized cost tracking across all providers via model_pricing.json
- **Deployment Options** - Podman Quadlet, Proxmox LXC, macOS hybrid dev stack, multi-stage Dockerfile

### Current Focus

- Documentation completeness and accuracy
- Stability improvements and bug fixes
- Provider compatibility (handling edge cases across DeepSeek, Z.AI, OpenAI, local models)
- Performance optimization for large document collections

## Planned Features

### Near Term
- Enhanced collaboration features
- Improved model support for emerging providers
- UI/UX refinements based on user feedback
- SearXNG setup documentation and configuration improvements

### Medium Term
- Plugin system for custom agents
- Advanced analytics and usage dashboards
- Real-time collaboration
- Extended document format support

### Long Term
- Distributed processing across multiple nodes
- Enterprise features (SSO, audit logging, role-based access)
- Mobile-friendly interface
- Advanced multi-modal research (image and video analysis)

## Contributing

We welcome contributions! Please see our [GitHub repository](https://github.com/murtaza-nasir/axiom) for:

- Issue tracking
- Feature requests
- Pull requests
- Discussions

## Community Feedback

Your feedback shapes our roadmap. Join the discussion:

- [GitHub Discussions](https://github.com/murtaza-nasir/axiom/discussions)
- [Issues](https://github.com/murtaza-nasir/axiom/issues)
