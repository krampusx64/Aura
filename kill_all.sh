#!/bin/bash

echo "Stopping AuraGo and Lifeboat..."
pkill -f aurago
pkill -f lifeboat

# Remove lock files if they exist
rm -f aurago.lock
rm -f lifeboat.lock

echo "All AuraGo processes stopped."
