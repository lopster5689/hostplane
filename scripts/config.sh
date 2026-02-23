cat > /root/.my-hosto.cnf << 'EOF'
[client]
user=control
password=***REMOVED***
host=***REMOVED***
database=controlplane
EOF
chmod 600 /root/.my-hosto.cnf
