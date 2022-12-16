package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

//*************************************************************************************************

func loadCA() (*x509.Certificate, []byte, *rsa.PrivateKey, error) {
	// if ca doesn't exist, create it and the private key
	_, err := os.Stat("./cert/ca.pem")
	if err != nil {
		return createCA()
	}

	// if private key doesn't exist, create it and the ca
	_, err = os.Stat("./cert/key.pem")
	if err != nil {
		return createCA()
	}

	// both files exist, so load them
	caPEM, err := os.ReadFile("./cert/ca.pem")
	if err != nil {
		return nil, nil, nil, err
	}
	caPrivKeyPEM, err := os.ReadFile("./cert/key.pem")
	if err != nil {
		return nil, nil, nil, err
	}
	block, _ := pem.Decode(caPrivKeyPEM)
	caPrivKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}

	der, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(der.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}

	return ca, caPEM, caPrivKey, nil
}

//*************************************************************************************************

func createCA() (*x509.Certificate, []byte, *rsa.PrivateKey, error) {
	fmt.Println("creating new Certificate Authority")
	max := int64(math.Pow(float64(2), float64(62)))
	serialNum, _ := rand.Int(rand.Reader, big.NewInt(max))

	ca := &x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			Organization:  []string{"Acme"},
			Country:       []string{"US"},
			Province:      []string{""},
			Locality:      []string{"Right Here"},
			StreetAddress: []string{"Somewhere"},
			PostalCode:    []string{"90210"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, nil, err
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, nil, err
	}

	caPEM := new(bytes.Buffer)
	pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	caPrivKeyPEM := new(bytes.Buffer)
	pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})

	err = os.WriteFile("./cert/ca.pem", caPEM.Bytes(), 0777)
	if err != nil {
		return nil, nil, nil, err
	}

	err = os.WriteFile("./cert/key.pem", caPrivKeyPEM.Bytes(), 0777)
	if err != nil {
		return nil, nil, nil, err
	}

	return ca, caPEM.Bytes(), caPrivKey, nil
}

//*************************************************************************************************

func createFakeCert(serverAddress string, ca *x509.Certificate, caPrivKey *rsa.PrivateKey) (*tls.Certificate, error) {
	hostname := strings.Split(serverAddress, ":")[0]
	max := int64(math.Pow(float64(2), float64(62)))
	serialNum, _ := rand.Int(rand.Reader, big.NewInt(max))
	//fmt.Println("creating fake cert for", hostname, "serialNum", serialNum)
	serverURL := url.URL{Scheme: "https", Host: serverAddress, Path: "*"}

	cert := &x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			Organization:  []string{"Fake Company"},
			Country:       []string{"US"},
			Province:      []string{""},
			Locality:      []string{"Nowhere"},
			StreetAddress: []string{"Huh"},
			PostalCode:    []string{"90210"},
		},
		DNSNames:     []string{hostname, "https://" + serverAddress},
		URIs:         []*url.URL{&serverURL},
		NotBefore:    time.Now().AddDate(0, -1, 0),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	certPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, err
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, cert, ca, &certPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, err
	}

	certPEM := new(bytes.Buffer)
	pem.Encode(certPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})

	certPrivKeyPEM := new(bytes.Buffer)
	pem.Encode(certPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(certPrivKey),
	})

	serverCert, err := tls.X509KeyPair(certPEM.Bytes(), certPrivKeyPEM.Bytes())
	if err != nil {
		return nil, err
	}

	return &serverCert, nil
}

//*************************************************************************************************

func upgradeToTLS(conn net.Conn, serverCert *tls.Certificate) (*tls.Conn, error) {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*serverCert},
	}

	tlsConn := tls.Server(conn, tlsConfig)
	tlsConn.SetDeadline(time.Now().Add(time.Minute))
	err := tlsConn.Handshake()
	return tlsConn, err
}
