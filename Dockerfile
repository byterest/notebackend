FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY note-backend .
RUN chmod +x note-backend && mkdir -p uploads
EXPOSE 8082
CMD ["./note-backend"]
