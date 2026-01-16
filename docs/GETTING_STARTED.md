# Getting Started 🚀

This guide walks you through setting up your own edge container registry from scratch. Should take about 15-20 minutes.

---

## Prerequisites

Before we begin, you'll need:

1. **A Fastly account** with Compute@Edge enabled
   - Sign up at [fastly.com/signup](https://www.fastly.com/signup/)
   - Compute is included in the free tier for testing

2. **Fastly CLI** installed
   ```bash
   # macOS
   brew install fastly/tap/fastly

   # Windows
   scoop install fastly

   # Linux (or any OS with npm)
   npm install -g @fastly/cli
   ```

3. **Go 1.23+** installed
   - Download from [golang.org/dl](https://golang.org/dl/)
   - Verify: `go version`

4. **Docker** (for testing)
   - [Get Docker](https://docs.docker.com/get-docker/)

---

## Step 1: Clone the Repository

```bash
git clone https://github.com/yourusername/edge-container-registry.git
cd edge-container-registry
```

Take a look around:
```bash
ls -la
# src/           - Go source code
# docs/          - Documentation
# fastly.toml    - Fastly configuration
# go.mod         - Go dependencies
```

---

## Step 2: Authenticate with Fastly

First, let's log in to Fastly:

```bash
fastly profile create
```

This will:
1. Open your browser to log in
2. Create an API token
3. Save it locally for future commands

Verify it worked:
```bash
fastly whoami
# Should show your account info
```

---

## Step 3: Create Fastly Resources

Your registry needs a few backend resources. Let's create them.

### 3.1 Create KV Stores

These store your image manifests and metadata:

```bash
# Store for manifests
fastly kv-store create --name=oci-registry-manifests
# Note the ID that's returned!

# Store for metadata (tags, catalog, upload sessions)
fastly kv-store create --name=oci-registry-metadata
# Note this ID too!
```

### 3.2 Create Object Storage

This stores the actual image layers (blobs). Do this in the Fastly dashboard:

1. Go to [manage.fastly.com](https://manage.fastly.com)
2. Navigate to **Resources** → **Object Storage**
3. Click **Create bucket**
4. Choose a region (pick one close to you for testing)
5. Note down:
   - Bucket name
   - Access Key ID
   - Secret Access Key
   - Endpoint URL (e.g., `eu-central.object.fastlystorage.app`)

### 3.3 Create Secret Store

This securely stores your Object Storage credentials:

```bash
fastly secret-store create --name=oci-registry-secrets
# Note the store ID!
```

Now add your credentials:

```bash
# Your Object Storage access key
fastly secret-store entry create \
  --store-id=YOUR_STORE_ID \
  --name=FASTLY_OS_ACCESS_KEY_ID \
  --secret="YOUR_ACCESS_KEY"

# Your Object Storage secret key
fastly secret-store entry create \
  --store-id=YOUR_STORE_ID \
  --name=FASTLY_OS_SECRET_ACCESS_KEY \
  --secret="YOUR_SECRET_KEY"
```

Optional - add authentication:
```bash
# Username for docker login
fastly secret-store entry create \
  --store-id=YOUR_STORE_ID \
  --name=REGISTRY_USERNAME \
  --secret="myuser"

# Password for docker login
fastly secret-store entry create \
  --store-id=YOUR_STORE_ID \
  --name=REGISTRY_PASSWORD \
  --secret="mypassword"
```

---

## Step 4: Configure the Project

### 4.1 Update fastly.toml

Open `fastly.toml` and update these values:

```toml
manifest_version = 3
name = "edge-container-registry"
language = "go"
service_id = ""  # Leave empty for first deploy, or add existing service ID

[scripts]
build = "GOARCH=wasm GOOS=wasip1 go build -o bin/main.wasm ./src"

# Link your KV stores (update with your store IDs)
[setup.kv_stores]
"oci-registry-manifests" = { empty = true }
"oci-registry-metadata" = { empty = true }

# Link your secret store
[setup.secret_stores]
"oci-registry-secrets" = {}
```

### 4.2 Update Object Storage Config

In `src/s3auth.go`, update these constants with your bucket info:

```go
const (
    FastlyOSHost   = "YOUR_REGION.object.fastlystorage.app"  // e.g., us-east, eu-central
    FastlyOSRegion = "YOUR_REGION"                           // Must match your bucket region
    S3Bucket       = "YOUR_BUCKET_NAME"                      // Your Object Storage bucket
)
```

---

## Step 5: Build and Deploy

### First, test locally:

```bash
# Build the WASM binary
fastly compute build

# Run local server
fastly compute serve
```

You should see:
```
✓ Running local server...
    Listening on http://127.0.0.1:7676
```

Test it:
```bash
curl http://localhost:7676/v2/
# Should return: {}
```

### Deploy to Fastly:

```bash
fastly compute deploy
```

During first deploy, you'll be asked:
- **Domain**: Choose a `.edgecompute.app` subdomain (e.g., `myregistry.edgecompute.app`)
- **Backend**: Configure the object storage backend

After deployment:
```
✓ Deployed!
    Domain: myregistry.edgecompute.app
```

---

## Step 6: Test Your Registry

### Check it's running:

```bash
curl https://myregistry.edgecompute.app/v2/
# Should return: {}
```

### Push an image:

```bash
# Pull a small test image
docker pull alpine:latest

# Tag it for your registry
docker tag alpine:latest myregistry.edgecompute.app/test/alpine:latest

# Push it!
docker push myregistry.edgecompute.app/test/alpine:latest
```

If you set up authentication:
```bash
docker login myregistry.edgecompute.app
# Enter username and password
```

### Pull it back:

```bash
# Remove local copy first
docker rmi myregistry.edgecompute.app/test/alpine:latest

# Pull from your registry
docker pull myregistry.edgecompute.app/test/alpine:latest
```

### Check what's stored:

```bash
# List repositories
curl https://myregistry.edgecompute.app/v2/_catalog
# {"repositories":["test/alpine"]}

# List tags
curl https://myregistry.edgecompute.app/v2/test/alpine/tags/list
# {"name":"test/alpine","tags":["latest"]}
```

**Congratulations! Your edge container registry is live! 🎉**

---

## Troubleshooting

### "connection refused" on local test

Make sure the local server is running:
```bash
fastly compute serve
```

### "UNAUTHORIZED" on push

If you configured auth, you need to login first:
```bash
docker login myregistry.edgecompute.app
```

Or disable auth by not setting `REGISTRY_USERNAME` and `REGISTRY_PASSWORD` in the secret store.

### "BLOB_UNKNOWN" after push

KV Store is eventually consistent. Wait a few seconds and try again.

### Push seems stuck

Large layers take time. The 2-minute timeout means very large layers (>500MB) might need multiple retries. Docker handles this automatically.

### "Access Denied" from Object Storage

Check your credentials:
1. Verify the secret store entries are correct
2. Make sure the access key has write permissions to the bucket

---

## Next Steps

- Read the [Architecture](ARCHITECTURE.md) doc to understand how it works
- Check [API Reference](API_REFERENCE.md) for all endpoints
- Review [Limitations](LIMITATIONS.md) for what doesn't work yet

---

## Quick Reference

### Useful commands

```bash
# View logs
fastly log-tail

# Update deployment
fastly compute build && fastly compute deploy

# Check service status
fastly service describe
```

### Important URLs

- Your registry: `https://YOUR_DOMAIN.edgecompute.app`
- Fastly dashboard: [manage.fastly.com](https://manage.fastly.com)
- Fastly docs: [developer.fastly.com](https://developer.fastly.com)

---

Need help? Open an issue on GitHub!
