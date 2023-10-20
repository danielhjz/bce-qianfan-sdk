#!/bin/bash
set -e

# build wheel
make clean
poetry build
mkdir output
mv dist/* output
rm -rf dist