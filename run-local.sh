#!/bin/bash

# Chargement de la configuration depuis le fichier .env
if [ -f .env ]; then
    echo "Chargement des variables d'environnement depuis .env..."
    set -a
    source .env
    set +a
else
    echo "Attention : fichier .env introuvable. Assurez-vous que les variables d'environnement sont définies."
fi

echo "Compiling Binary..."
go build -o demo-agent ./cmd/demo

if [ $? -eq 0 ]; then
    echo "--------------------------------------------------"
    echo "Starting Local Agent Server on http://localhost:$PORT"
    echo "--------------------------------------------------"
    ./demo-agent
else
    echo "Build failed!"
fi
