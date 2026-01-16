# Contributing 🤝

Thanks for wanting to contribute! This project is open to everyone.

---

## Ways to Contribute

### Report Bugs

Found something broken? Open an issue with:

1. What you were trying to do
2. What happened instead
3. Steps to reproduce
4. Any error messages

### Suggest Features

Have an idea? Open an issue and describe:

1. What problem it solves
2. How it might work
3. Any alternatives you considered

### Submit Code

Want to fix a bug or add a feature? Here's how:

---

## Development Setup

### Prerequisites

- Go 1.23+
- Fastly CLI
- Docker (for testing)

### Clone and Build

```bash
git clone https://github.com/yourusername/edge-container-registry.git
cd edge-container-registry

# Build
fastly compute build

# Run locally
fastly compute serve
```

### Test Your Changes

```bash
# Check the API
curl http://localhost:7676/v2/

# Test with Docker
docker tag alpine:latest localhost:7676/test:latest
docker push localhost:7676/test:latest
docker pull localhost:7676/test:latest
```

---

## Code Style

### Go Formatting

We use standard Go formatting:

```bash
go fmt ./src/...
```

### File Organization

```
src/
├── main.go          # Entry point, routing
├── manifests.go     # Manifest operations
├── blobs.go         # Blob operations
├── uploads.go       # Upload handling
├── multipart.go     # S3 multipart
├── s3auth.go        # AWS signing
├── auth.go          # Authentication
├── validation.go    # Input validation
├── cosmetics.go     # Response formatting
└── oci11.go         # OCI 1.1 features
```

### Comments

- Add comments for non-obvious code
- Document public functions
- Explain the "why", not just the "what"

---

## Pull Request Process

### 1. Fork and Branch

```bash
# Fork on GitHub, then:
git clone https://github.com/YOUR_USERNAME/edge-container-registry.git
cd edge-container-registry
git checkout -b my-feature
```

### 2. Make Your Changes

- Keep commits focused and atomic
- Write clear commit messages
- Test your changes locally

### 3. Push and PR

```bash
git push origin my-feature
```

Then open a Pull Request on GitHub.

### 4. Review

We'll review your PR and may ask for changes. Don't worry, this is normal!

---

## Commit Messages

Keep them clear and descriptive:

```
Good:
- "Add digest verification on upload completion"
- "Fix KV store timeout handling"
- "Update README with new setup instructions"

Not so good:
- "fix bug"
- "updates"
- "wip"
```

---

## What We're Looking For

Priority areas for contribution:

### High Priority

- [ ] Bearer token authentication (OAuth2 flow)
- [ ] Digest verification on upload
- [ ] Garbage collection for orphaned blobs
- [ ] Better error messages

### Medium Priority

- [ ] Rate limiting
- [ ] Audit logging
- [ ] Metrics endpoint
- [ ] Range request support

### Nice to Have

- [ ] Web UI for browsing
- [ ] Image signing support
- [ ] Multi-tenancy
- [ ] Retention policies

---

## Questions?

- Open an issue for questions
- Check existing issues first
- Be patient - we're all volunteers here

---

## Code of Conduct

Be kind. Be respectful. We're all here to learn and build cool stuff.

---

Thanks for contributing! 🚀
