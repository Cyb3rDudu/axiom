# CLI Commands Reference

AXIOM provides powerful command-line tools for bulk document processing, user management, and system administration. The CLI features direct processing with real-time progress feedback.

## Overview

The AXIOM CLI offers:

- **Direct Processing** - Documents processed immediately with live feedback
- **Real-time Progress** - See each step with timestamps
- **Multi-format Support** - PDF, Word, Markdown files
- **GPU Control** - Specify which GPU device to use
- **Bulk Operations** - Process entire directories
- **User Management** - Create and manage users
- **Document Organization** - Create and manage document groups

## Getting Started

### Platform-Specific Usage

#### Linux/macOS
```bash
# Make executable (first time only)
chmod +x axiom-cli.sh

# Show help
./axiom-cli.sh help
```

#### Windows PowerShell
```powershell
# Show help
.\axiom-cli.ps1 help
```

#### Windows Command Prompt
```cmd
REM Show help
axiom-cli.bat help
```

#### Direct Docker Execution
```bash
# Run CLI commands directly
docker exec axiom-backend python cli.py --help
```

## User Management Commands

### create-user

Create a new user account.

**Syntax:**
```bash
./axiom-cli.sh create-user <username> <password> [options]
```

**Options:**

- `--full-name "Name"` - Set user's full name
- `--admin` - Create admin user

**Examples:**
```bash
# Create regular user
./axiom-cli.sh create-user researcher pass123 --full-name "Research User"

# Create admin user
./axiom-cli.sh create-user admin adminpass --admin --full-name "Administrator"
```

## Document Group Management

### create-group

Create a document group for organization.

**Syntax:**
```bash
./axiom-cli.sh create-group <username> <group_name> [options]
```

**Options:**
- `--description "Text"` - Add group description

**Example:**
```bash
./axiom-cli.sh create-group researcher "AI Papers" \
  --description "Machine Learning Research Papers"
```

### list-groups

List document groups.

```bash
# List all groups
./axiom-cli.sh list-groups

# List groups for specific user
./axiom-cli.sh list-groups --user researcher
```

**Output:**
```
Group ID: abc123-def456
Name: AI Papers
Owner: researcher
Documents: 42
Description: Machine Learning Research Papers
```

**Note:** Document groups can be created and listed via CLI. Documents can be added to groups during ingestion with the `--group` flag. For other group management operations, use the web interface.

## Document Processing Commands

### ingest

Process documents with live feedback. Primary command for adding documents.

**Syntax:**
```bash
./axiom-cli.sh ingest <username> <directory> [options]
```

**Options:**

- `--group <group_id>` - Add to specific group
- `--force-reembed` - Force re-processing (default behavior skips already processed files)
- `--device <device>` - GPU device (cuda:0, cpu)
- `--delete-after-success` - Remove source files after processing
- `--batch-size <num>` - Parallel processing count

**Supported Formats:**

- PDF files (`.pdf`)
- Word documents (`.docx`, `.doc`)
- Markdown files (`.md`, `.markdown`)

### Mounting Directories for Batch Document Processing

The CLI service needs access to your documents. By default, only `./pdfs` is mounted. To process documents from other directories, you need to mount them in docker-compose.yml:

Edit the `cli` service in docker-compose.yml:

```yaml
cli:
  # ... existing configuration ...
  volumes:
    # ... existing volumes ...
    - ./pdfs:/app/pdfs  # Default PDF directory
    - ./documents:/app/documents  # Add your custom document directory
    - ./research-papers:/app/research-papers  # Another example
```

Then use the mounted path:
```bash
./axiom-cli.sh ingest researcher /app/documents
./axiom-cli.sh ingest researcher /app/research-papers
```

### Batch Document Processing Examples

**Examples:**
```bash
# Basic document ingestion from default directory
./axiom-cli.sh ingest researcher ./pdfs

# Process documents from custom mounted directory
./axiom-cli.sh ingest researcher ./documents

# Add to group with GPU selection
./axiom-cli.sh ingest researcher ./research-papers \
  --group abc123 --device cuda:0

# Process and cleanup temporary files
./axiom-cli.sh ingest researcher ./temp-docs \
  --delete-after-success

# Force re-processing of updated documents
./axiom-cli.sh ingest researcher ./updated-docs \
  --force-reembed

# Process with custom batch size for large collections
./axiom-cli.sh ingest researcher ./large-collection \
  --batch-size 10
# Make sure you have enough VRAM; adjust to lower batch size if you see out of memory errors
```

**Progress Output:**
```
Processing: paper1.pdf
[12:34:56] Converting to markdown...
[12:35:02] Extracting metadata...
[12:35:05] Generating embeddings...
[12:35:12] ✓ Successfully processed: paper1.pdf

Processing: paper2.pdf
[12:35:13] Converting to markdown...
```

## Search Commands

### search

Search documents using semantic search.

```bash
./axiom-cli.sh search <username> "search query" [options]
```

**Options:**

- `--limit <num>` - Result count (default: 10)
- `--group <group_id>` - Search within group
- `--threshold <float>` - Similarity threshold (0-1)

**Examples:**
```bash
# Basic search
./axiom-cli.sh search admin "quantum computing applications"

# Search with options
./axiom-cli.sh search researcher "machine learning" \
  --limit 20 \
  --group abc123 \
  --threshold 0.7
```

**Note:** Metadata search is available through the web interface. The CLI `search` command provides semantic search capabilities.

## System Management Commands

### status

Check document processing status (not system status).

```bash
# Check status for a user
./axiom-cli.sh status --user researcher

# Check status for a group
./axiom-cli.sh status --group <group_id>
```

**Note:** For system-wide statistics, use `reset-db --stats` command.

### cleanup

Clean up documents with specific status.

```bash
# Clean up failed documents
./axiom-cli.sh cleanup --user researcher --status failed

# Clean up for specific group
./axiom-cli.sh cleanup --group <group_id>

# Skip confirmation
./axiom-cli.sh cleanup --confirm
```

### cleanup-cli

Clean up documents stuck in CLI processing.

```bash
# Dry run to see what would be deleted
./axiom-cli.sh cleanup-cli --dry-run

# Force cleanup without confirmation
./axiom-cli.sh cleanup-cli --force
```

### reset-db

Database reset operations.

```bash
# Check database status
./axiom-cli.sh reset-db --stats

# Check consistency
./axiom-cli.sh reset-db --check

# Reset with backup
./axiom-cli.sh reset-db --backup

# Force reset (skip confirmation)
./axiom-cli.sh reset-db --force
```

**Note:** Backup and restore operations are handled through `reset-db --backup` or manual database operations. See the [Database Reset Guide](database-reset.md) for details.

## Performance Tips

### GPU Utilization

```bash
# Check available GPUs
nvidia-smi

# Use specific GPU
./axiom-cli.sh ingest researcher ./docs --device cuda:0

# Use multiple GPUs (process in parallel)
./axiom-cli.sh ingest researcher ./docs1 --device cuda:0 &
./axiom-cli.sh ingest researcher ./docs2 --device cuda:1 &
wait
```

### Batch Size Optimization

```bash
# Small files, increase batch size
./axiom-cli.sh ingest researcher ./small-docs --batch-size 10

# Large files, decrease batch size
./axiom-cli.sh ingest researcher ./large-pdfs --batch-size 2
```

### Memory Management

```bash
# Limit memory usage
export PYTORCH_CUDA_ALLOC_CONF=max_split_size_mb:512
./axiom-cli.sh ingest researcher ./documents
```

## Troubleshooting

### Common Issues

**Permission Denied:**
```bash
# Fix permissions
chmod +x axiom-cli.sh
```

**Docker Not Running:**
```bash
# Start Docker services
docker compose up -d
```

**GPU Not Available:**
```bash
# Force CPU mode
./axiom-cli.sh ingest researcher ./docs --device cpu
```

**Out of Memory:**
```bash
# Reduce batch size
./axiom-cli.sh ingest researcher ./docs --batch-size 1
```

### Log Files

Check logs for errors:

```bash
# View backend logs
docker compose logs axiom-backend --tail=100
```

## Next Steps

- [Database Management](database-reset.md) - Database operations
- [User Guide](../../user-guide/index.md) - Using the web interface
- [Troubleshooting](../../troubleshooting/index.md) - Common issues
- [API Reference](#) - REST API documentation