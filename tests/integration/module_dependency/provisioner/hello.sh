#!/bin/sh
# Copies greeting.txt to ran.txt beside $0.
cp "$(dirname "$0")/greeting.txt" "$(dirname "$0")/ran.txt"
