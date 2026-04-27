FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/notty-backend ./backend/cmd/server

FROM alpine:3.20
WORKDIR /app
RUN mkdir -p /data
COPY --from=build /bin/notty-backend /usr/local/bin/notty-backend
EXPOSE 8080
CMD ["notty-backend"]
