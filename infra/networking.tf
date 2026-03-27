resource "aws_vpc" "hopvault" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "hopvault-vpc"
  }
}

resource "aws_internet_gateway" "hopvault" {
  vpc_id = aws_vpc.hopvault.id

  tags = {
    Name = "hopvault-igw"
  }
}

resource "aws_subnet" "public_az1" {
  vpc_id                  = aws_vpc.hopvault.id
  cidr_block              = local.public_subnet_cidrs.public_az1
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "hopvault-public-az1"
  }
}

resource "aws_subnet" "public_az2" {
  vpc_id                  = aws_vpc.hopvault.id
  cidr_block              = local.public_subnet_cidrs.public_az2
  availability_zone       = data.aws_availability_zones.available.names[1]
  map_public_ip_on_launch = true

  tags = {
    Name = "hopvault-public-az2"
  }
}

resource "aws_subnet" "private_az1" {
  vpc_id            = aws_vpc.hopvault.id
  cidr_block        = local.private_subnet_cidrs.private_az1
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "hopvault-private-az1"
  }
}

resource "aws_subnet" "private_az2" {
  vpc_id            = aws_vpc.hopvault.id
  cidr_block        = local.private_subnet_cidrs.private_az2
  availability_zone = data.aws_availability_zones.available.names[1]

  tags = {
    Name = "hopvault-private-az2"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.hopvault.id

  tags = {
    Name = "hopvault-public-rt"
  }
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.hopvault.id
}

resource "aws_route_table_association" "public_az1" {
  subnet_id      = aws_subnet.public_az1.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "public_az2" {
  subnet_id      = aws_subnet.public_az2.id
  route_table_id = aws_route_table.public.id
}

resource "aws_eip" "nat_az1" {
  domain = "vpc"

  tags = {
    Name = "hopvault-nat-eip-az1"
  }
}

resource "aws_eip" "nat_az2" {
  domain = "vpc"

  tags = {
    Name = "hopvault-nat-eip-az2"
  }
}

resource "aws_nat_gateway" "az1" {
  allocation_id = aws_eip.nat_az1.id
  subnet_id     = aws_subnet.public_az1.id

  tags = {
    Name = "hopvault-nat-az1"
  }

  depends_on = [aws_internet_gateway.hopvault]
}

resource "aws_nat_gateway" "az2" {
  allocation_id = aws_eip.nat_az2.id
  subnet_id     = aws_subnet.public_az2.id

  tags = {
    Name = "hopvault-nat-az2"
  }

  depends_on = [aws_internet_gateway.hopvault]
}

resource "aws_route_table" "private_az1" {
  vpc_id = aws_vpc.hopvault.id

  tags = {
    Name = "hopvault-private-rt-az1"
  }
}

resource "aws_route" "private_az1_egress" {
  route_table_id         = aws_route_table.private_az1.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.az1.id
}

resource "aws_route_table_association" "private_az1" {
  subnet_id      = aws_subnet.private_az1.id
  route_table_id = aws_route_table.private_az1.id
}

resource "aws_route_table" "private_az2" {
  vpc_id = aws_vpc.hopvault.id

  tags = {
    Name = "hopvault-private-rt-az2"
  }
}

resource "aws_route" "private_az2_egress" {
  route_table_id         = aws_route_table.private_az2.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.az2.id
}

resource "aws_route_table_association" "private_az2" {
  subnet_id      = aws_subnet.private_az2.id
  route_table_id = aws_route_table.private_az2.id
}
