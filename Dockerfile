# Use an official Golang image to build the application
FROM golang:1.23 AS builder

# Set the working directory inside the builder container
WORKDIR /awesomeProject3

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Set Go proxy
ENV GOPROXY=https://proxy.golang.org

# Download all dependencies
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the builder container
COPY . .

# Install necessary build dependencies for SQLite
RUN apt-get update && apt-get install -y gcc musl-dev

# Build the application
RUN go build -o awesomeProject3-app main.go

# Use an Ubuntu base image to run the built binary
FROM ubuntu:latest

# Create necessary directories for media
RUN mkdir -p /media/audio /media/video /media/image /media/document

# Install necessary runtime dependencies for SQLite and GLIBC
RUN apt-get update && apt-get install -y libc6 libc6-dev sqlite3 ca-certificates

# Copy the built binary from the builder stage to the minimal base image
COPY --from=builder /awesomeProject3/awesomeProject3-app /awesomeProject3-app

# Copy custom certificates to the container
COPY web_whatsapp_cert.pem /usr/local/share/ca-certificates/web_whatsapp_cert.pem

# Update CA certificates
RUN update-ca-certificates

# Set the necessary permissions
RUN chmod +x /awesomeProject3-app

# Set environment variable for Docker mode
ENV DOCKER_MODE=true

# Define a volume for the session data
VOLUME ["/data"]

# Run the application
CMD ["/awesomeProject3-app"]
