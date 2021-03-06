package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/big"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	caName     string
	ecdsaCurve string
	ed25519Key bool
	rsaBits    int
	rsaKey     bool
	showExp    bool
)

func main() {
	err := main2()
	if err != nil {
		log.Fatal(err)
	}
}

type issuer struct {
	key  interface{}
	cert *x509.Certificate
}

func getIssuer(keyFile, certFile string) (*issuer, error) {
	keyContents, keyErr := ioutil.ReadFile(keyFile)
	certContents, certErr := ioutil.ReadFile(certFile)
	if os.IsNotExist(keyErr) && os.IsNotExist(certErr) {
		err := makeIssuer(keyFile, certFile)
		if err != nil {
			return nil, err
		}
		return getIssuer(keyFile, certFile)
	} else if keyErr != nil {
		return nil, fmt.Errorf("%s (but %s exists)", keyErr, certFile)
	} else if certErr != nil {
		return nil, fmt.Errorf("%s (but %s exists)", certErr, keyFile)
	}
	key, err := readPrivateKey(keyContents)
	if err != nil {
		return nil, fmt.Errorf("reading private key from %s: %s", keyFile, err)
	}
	pubKey := publicKey(key)

	cert, err := parseCert(certContents)
	if err != nil {
		return nil, fmt.Errorf("reading CA certificate from %s: %s", certFile, err)
	}

	equal, err := publicKeysEqual(pubKey, cert.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("comparing public keys: %s", err)
	} else if !equal {
		return nil, fmt.Errorf("public key in CA certificate %s doesn't match private key in %s",
			certFile, keyFile)
	}
	return &issuer{key, cert}, nil
}

func readPrivateKey(keyContents []byte) (interface{}, error) {
	block, _ := pem.Decode(keyContents)
	if block == nil {
		return nil, fmt.Errorf("no PEM found")
	} else if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("incorrect PEM type %s", block.Type)
	}
	return x509.ParsePKCS8PrivateKey(block.Bytes)
}

func readCert(certPath string) (*x509.Certificate, error) {
	certContents, err := ioutil.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading certificate from %s: %s", certPath, err)
	}
	return parseCert(certContents)
}

func parseCert(certContents []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certContents)
	if block == nil {
		return nil, fmt.Errorf("no PEM found")
	} else if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("incorrect PEM type %s", block.Type)
	}
	return x509.ParseCertificate(block.Bytes)
}

func makeIssuer(keyFile, certFile string) error {
	key, err := makeKey(keyFile)
	if err != nil {
		return err
	}
	_, err = makeRootCert(key, certFile)
	if err != nil {
		return err
	}
	return nil
}

func makeKey(filename string) (interface{}, error) {
	var err error
	var key crypto.PrivateKey
	var der []byte

	if ed25519Key || rsaKey {
		if ed25519Key {
			_, key, err = ed25519.GenerateKey(rand.Reader)
		} else {
			key, err = rsa.GenerateKey(rand.Reader, rsaBits)
		}
	} else {
		switch ecdsaCurve {
		case "P224":
			key, err = ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
		case "P256":
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		case "P384":
			key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		case "P521":
			key, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		default:
			return nil, fmt.Errorf("unrecognized curve: %q", ecdsaCurve)
		}
	}

	if err != nil {
		return nil, err
	}

	der, err = x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	err = pem.Encode(file, &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

func makeRootCert(key interface{}, filename string) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return nil, err
	}

	pubKey := publicKey(key)

	skid, err := calculateSKID(pubKey)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		Subject: pkix.Name{
			CommonName: caName,
		},
		SerialNumber: serial,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(100, 0, 0),

		SubjectKeyId:          skid,
		AuthorityKeyId:        skid,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pubKey, key)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	err = pem.Encode(file, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func parseIPs(ipAddresses []string) ([]net.IP, error) {
	var parsed []net.IP
	for _, s := range ipAddresses {
		p := net.ParseIP(s)
		if p == nil {
			return nil, fmt.Errorf("invalid IP address %s", s)
		}
		parsed = append(parsed, p)
	}
	return parsed, nil
}

func publicKeysEqual(a, b interface{}) (bool, error) {
	aBytes, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false, err
	}
	bBytes, err := x509.MarshalPKIXPublicKey(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aBytes, bBytes), nil
}

func calculateSKID(pubKey crypto.PublicKey) ([]byte, error) {
	spkiASN1, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return nil, err
	}

	var spki struct {
		Algorithm        pkix.AlgorithmIdentifier
		SubjectPublicKey asn1.BitString
	}
	_, err = asn1.Unmarshal(spkiASN1, &spki)
	if err != nil {
		return nil, err
	}
	skid := sha1.Sum(spki.SubjectPublicKey.Bytes)
	return skid[:], nil
}

func publicKey(privKey interface{}) interface{} {
	switch k := privKey.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public().(ed25519.PublicKey)
	}
	return nil
}

func sign(iss *issuer, domains []string, ipAddresses []string) (*x509.Certificate, error) {
	var cn string
	if len(domains) > 0 {
		cn = domains[0]
	} else if len(ipAddresses) > 0 {
		cn = ipAddresses[0]
	} else {
		return nil, fmt.Errorf("must specify at least one domain name or IP address")
	}
	var cnFolder = strings.Replace(cn, "*", "_", -1)
	err := os.Mkdir(cnFolder, 0700)
	if err != nil && !os.IsExist(err) {
		return nil, err
	}
	key, err := makeKey(fmt.Sprintf("%s/key.pem", cnFolder))
	if err != nil {
		return nil, err
	}
	pubKey := publicKey(key)
	parsedIPs, err := parseIPs(ipAddresses)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		DNSNames:    domains,
		IPAddresses: parsedIPs,
		Subject: pkix.Name{
			CommonName: cn,
		},
		SerialNumber: serial,
		NotBefore:    time.Now(),
		// Set the validity period to 2 years and 30 days, to satisfy the iOS and
		// macOS requirements that all server certificates must have validity
		// shorter than 825 days:
		// https://derflounder.wordpress.com/2019/06/06/new-tls-security-requirements-for-ios-13-and-macos-catalina-10-15/
		NotAfter: time.Now().AddDate(2, 0, 30),

		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	if !ed25519Key && ecdsaCurve == "" {
		template.KeyUsage |= x509.KeyUsageKeyEncipherment
	}

	der, err := x509.CreateCertificate(rand.Reader, template, iss.cert, pubKey, iss.key)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(fmt.Sprintf("%s/cert.pem", cnFolder), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	err = pem.Encode(file, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func split(s string) (results []string) {
	if len(s) > 0 {
		return strings.Split(s, ",")
	}
	return nil
}

func main2() error {
	var caKey = flag.String("ca-key", "microca-key.pem", "Root private key filename, PEM encoded.")
	var caCert = flag.String("ca-cert", "microca.pem", "Root certificate filename, PEM encoded.")
	var domains = flag.String("domains", "", "Comma separated domain names to include as Server Alternative Names.")
	var ipAddresses = flag.String("ip-addresses", "", "Comma separated IP addresses to include as Server Alternative Names.")
	flag.BoolVar(&ed25519Key, "ed25519", false, "Generate ED25519 keys")
	flag.BoolVar(&rsaKey, "rsa", false, "Generate RSA keys")
	flag.BoolVar(&showExp, "show-expire", false, "Show the expiration date for each certificate.")
	flag.IntVar(&rsaBits, "rsa-bits", 4096, "RSA key size in bits.")
	flag.StringVar(&ecdsaCurve, "ecdsa-curve", "P256", "ECDSA curve used when generating keys (P224, P256 (default), P384, P521).")
	flag.StringVar(&caName, "ca-name", "microca root", "Common Name used in root certificate.")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, `
microca is a simple CA intended for use in situations where the CA operator
also operates each host where a certificate will be used. It automatically
generates both a key and a certificate when asked to produce a certificate.
It does not offer OCSP or CRL services. microca is appropriate, for instance,
for generating certificates for RPC systems or microservices.

On first run, microca will generate a keypair and a root certificate in the
current directory, and will reuse that same keypair and root certificate
unless they are deleted.

On each run, microca will generate a new keypair and sign an end-entity (leaf)
certificate for that keypair. The certificate will contain a list of DNS names
and/or IP addresses from the command line flags. The key and certificate are
placed in a new directory whose name is chosen as the first domain name from
the certificate, or the first IP address if no domain names are present. It
will not overwrite existing keys or certificates.

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if showExp {
		afp, err := filepath.Abs(".")
		if err != nil {
			return err
		}
		caW := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)

		fmt.Fprintf(caW, "CA Certificate\tType\tExpiration\n")
		fmt.Fprintf(w, "Leaf Certificate\tType\tExpiration\n")

		topCerts, err := filepath.Glob("./*.pem")
		if err != nil {
			return err
		}

		for _, tc := range topCerts {
			if strings.Contains(tc, "key.pem") {
				continue
			}
			cert, err := readCert(tc)
			if err != nil {
				return err
			}

			fmt.Fprintf(caW, "%s (%s)\t%s\t%s\n",
				cert.Subject,
				tc,
				cert.PublicKeyAlgorithm,
				cert.NotAfter,
			)
		}
		fmt.Fprintf(caW, "\t\n")

		// Prints CA cert info first, and leaf second.
		defer w.Flush()
		defer caW.Flush()

		err = filepath.Walk(afp, func(fpath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				certFile := path.Join(info.Name(), "cert.pem")
				if _, err := os.Stat(certFile); err == nil {
					cert, err := readCert(certFile)
					if err != nil {
						return err
					}

					fmt.Fprintf(w, "%s\t%s\t%s\n",
						strings.Join(cert.DNSNames, ", "),
						cert.PublicKeyAlgorithm,
						cert.NotAfter,
					)
				}
			}
			return nil
		})
		if err != nil {
			log.Println(err)
		}
		return nil
	}

	if *domains == "" && *ipAddresses == "" {
		flag.Usage()
		os.Exit(1)
	}

	if len(flag.Args()) > 0 {
		fmt.Printf("Extra arguments: %s (maybe there are spaces in your domain list?)\n", flag.Args())
		os.Exit(1)
	}

	domainSlice := split(*domains)
	domainRe := regexp.MustCompile("^[A-Za-z0-9.*-]+$")
	for _, d := range domainSlice {
		if !domainRe.MatchString(d) {
			fmt.Printf("Invalid domain name %q\n", d)
			os.Exit(1)
		}
	}

	ipSlice := split(*ipAddresses)
	for _, ip := range ipSlice {
		if net.ParseIP(ip) == nil {
			fmt.Printf("Invalid IP address %q\n", ip)
			os.Exit(1)
		}
	}

	issuer, err := getIssuer(*caKey, *caCert)
	if err != nil {
		return err
	}

	_, err = sign(issuer, domainSlice, ipSlice)
	return err
}
