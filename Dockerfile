FROM golang:alpine AS sherpa

WORKDIR /app
COPY . .
RUN go build -o main .

CMD /app/main
