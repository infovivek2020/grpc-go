/*
 *
 * Copyright 2015 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package main implements a client for Greeter service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/compressor/gzip"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
)

const (
	defaultName = "world"
)

var (
	addr = flag.String("addr", "localhost:50051", "the address to connect to")
	name = flag.String("name", defaultName, "Name to greet")
)

func main() {
	flag.Parse()
	// ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// defer cancel()

	// Establish a new gRPC client connection using grpc.NewClient
	client, err := grpc.NewClient(
		*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), // Insecure credentials for non-TLS connection
		grpc.WithDefaultCallOptions(
			grpc.UseCompressor(gzip.Name),            // Use gzip compressor
			grpc.MaxCallRecvMsgSize(math.MaxInt64-1), // Set maximum receive message size
		),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  1.0 * time.Second, // Initial delay before retrying
				Multiplier: 1.6,               // Backoff multiplier for successive retries
				Jitter:     0.2,               // Jitter to randomize backoff
				MaxDelay:   120 * time.Second, // Maximum delay between retries
			},
			MinConnectTimeout: 5 * time.Second, // Minimum time to spend on connection attempts
		}),
	)
	if err != nil {
		log.Fatalf("failed to create gRPC client: %v", err)
	}
	defer client.Close()

	// Check the connection state and ensure it's ready
	state := client.GetState()
	log.Printf("Connection state: %v", state)

	if state != connectivity.Ready {
		log.Fatalf("Connection not ready: %v", state)
	}

	// Create a new instance of your service client
	myServiceClient := pb.NewGreeterClient(client)

	// Create a context with a timeout for your request
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer reqCancel()

	// Example RPC call to your service

	response, err := myServiceClient.SayHello(reqCtx, &pb.HelloRequest{Name: *name})
	if err != nil {
		log.Fatalf("could not call YourMethod: %v", err)
	}

	// Handle the response
	fmt.Printf("Response from server: %s\n", response.GetMessage())
}
